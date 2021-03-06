/*
 * Copyright 2017-2018 Dgraph Labs, Inc.
 *
 * This file is available under the Apache License, Version 2.0,
 * with the Commons Clause restriction.
 */

package bulk

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dgraph-io/dgo/protos/api"
	"github.com/dgraph-io/dgraph/gql"
	"github.com/dgraph-io/dgraph/posting"
	"github.com/dgraph-io/dgraph/protos/intern"
	"github.com/dgraph-io/dgraph/rdf"
	"github.com/dgraph-io/dgraph/tok"
	"github.com/dgraph-io/dgraph/types"
	"github.com/dgraph-io/dgraph/types/facets"
	"github.com/dgraph-io/dgraph/x"
	farm "github.com/dgryski/go-farm"
	"github.com/gogo/protobuf/proto"
	"github.com/pkg/errors"
)

type mapper struct {
	*state
	shards []shardState // shard is based on predicate
}

type shardState struct {
	// Buffer up map entries until we have a sufficient amount, then sort and
	// write them to file.
	entriesBuf []byte
	mu         sync.Mutex // Allow only 1 write per shard at a time.
}

func newMapper(st *state) *mapper {
	return &mapper{
		state:  st,
		shards: make([]shardState, st.opt.MapShards),
	}
}

func less(lhs, rhs *intern.MapEntry) bool {
	if keyCmp := bytes.Compare(lhs.Key, rhs.Key); keyCmp != 0 {
		return keyCmp < 0
	}
	lhsUID := lhs.Uid
	rhsUID := rhs.Uid
	if lhs.Posting != nil {
		lhsUID = lhs.Posting.Uid
	}
	if rhs.Posting != nil {
		rhsUID = rhs.Posting.Uid
	}
	return lhsUID < rhsUID
}

func (m *mapper) writeMapEntriesToFile(entriesBuf []byte, shardIdx int) {
	defer m.shards[shardIdx].mu.Unlock() // Locked by caller.

	buf := entriesBuf
	var entries []*intern.MapEntry
	for len(buf) > 0 {
		sz, n := binary.Uvarint(buf)
		x.AssertTrue(n > 0)
		buf = buf[n:]
		me := new(intern.MapEntry)
		x.Check(proto.Unmarshal(buf[:sz], me))
		buf = buf[sz:]
		entries = append(entries, me)
	}

	sort.Slice(entries, func(i, j int) bool {
		return less(entries[i], entries[j])
	})

	buf = entriesBuf
	for _, me := range entries {
		n := binary.PutUvarint(buf, uint64(me.Size()))
		buf = buf[n:]
		n, err := me.MarshalTo(buf)
		x.Check(err)
		buf = buf[n:]
	}
	x.AssertTrue(len(buf) == 0)

	fileNum := atomic.AddUint32(&m.mapFileId, 1)
	filename := filepath.Join(
		m.opt.TmpDir,
		"shards",
		fmt.Sprintf("%03d", shardIdx),
		fmt.Sprintf("%06d.map", fileNum),
	)
	x.Check(os.MkdirAll(filepath.Dir(filename), 0755))
	x.Check(x.WriteFileSync(filename, entriesBuf, 0644))
}

func (m *mapper) run() {
	for chunkBuf := range m.rdfChunkCh {
		done := false
		for !done {
			rdf, err := chunkBuf.ReadString('\n')
			if err == io.EOF {
				// Process the last RDF rather than breaking immediately.
				done = true
			} else {
				x.Check(err)
			}
			rdf = strings.TrimSpace(rdf)

			// process RDF line
			if err := m.processRDF(rdf); err != nil {
				atomic.AddInt64(&m.prog.errCount, 1)
				if !m.opt.IgnoreErrors {
					x.Check(err)
				}
			}

			atomic.AddInt64(&m.prog.rdfCount, 1)
			for i := range m.shards {
				sh := &m.shards[i]
				if len(sh.entriesBuf) >= int(m.opt.MapBufSize) {
					sh.mu.Lock() // One write at a time.
					go m.writeMapEntriesToFile(sh.entriesBuf, i)
					sh.entriesBuf = make([]byte, 0, m.opt.MapBufSize*11/10)
				}
			}
		}
	}
	for i := range m.shards {
		sh := &m.shards[i]
		if len(sh.entriesBuf) > 0 {
			sh.mu.Lock() // One write at a time.
			m.writeMapEntriesToFile(sh.entriesBuf, i)
		}
		m.shards[i].mu.Lock() // Ensure that the last file write finishes.
	}
}

func (m *mapper) addMapEntry(key []byte, p *intern.Posting, shard int) {
	atomic.AddInt64(&m.prog.mapEdgeCount, 1)

	me := &intern.MapEntry{
		Key: key,
	}
	if p.PostingType != intern.Posting_REF || len(p.Facets) > 0 {
		me.Posting = p
	} else {
		me.Uid = p.Uid
	}
	sh := &m.shards[shard]

	var err error
	sh.entriesBuf = x.AppendUvarint(sh.entriesBuf, uint64(me.Size()))
	sh.entriesBuf, err = x.AppendProtoMsg(sh.entriesBuf, me)
	x.Check(err)
}

func (m *mapper) processRDF(rdfLine string) error {
	nq, err := parseNQuad(rdfLine)
	if err != nil {
		if err == rdf.ErrEmpty {
			return nil
		}
		return errors.Wrapf(err, "while parsing line %q", rdfLine)
	}
	if err := facets.SortAndValidate(nq.Facets); err != nil {
		return err
	}
	m.processNQuad(nq)
	return nil
}

func (m *mapper) processNQuad(nq gql.NQuad) {
	sid := m.lookupUid(nq.GetSubject())
	var oid uint64
	var de *intern.DirectedEdge
	if nq.GetObjectValue() == nil {
		oid = m.lookupUid(nq.GetObjectId())
		de = nq.CreateUidEdge(sid, oid)
	} else {
		var err error
		de, err = nq.CreateValueEdge(sid)
		x.Check(err)
	}

	fwd, rev := m.createPostings(nq, de)
	shard := m.state.shards.shardFor(nq.Predicate)
	key := x.DataKey(nq.Predicate, sid)
	m.addMapEntry(key, fwd, shard)

	if rev != nil {
		key = x.ReverseKey(nq.Predicate, oid)
		m.addMapEntry(key, rev, shard)
	}
	m.addIndexMapEntries(nq, de)

	if m.opt.ExpandEdges {
		shard := m.state.shards.shardFor("_predicate_")
		key = x.DataKey("_predicate_", sid)
		pp := m.createPredicatePosting(nq.Predicate)
		m.addMapEntry(key, pp, shard)
	}
}

func (m *mapper) lookupUid(xid string) uint64 {
	uid, isNew := m.xids.AssignUid(xid)
	if !isNew || !m.opt.StoreXids {
		return uid
	}
	if strings.HasPrefix(xid, "_:") {
		// Don't store xids for blank nodes.
		return uid
	}
	nq := gql.NQuad{&api.NQuad{
		Subject:   xid,
		Predicate: "xid",
		ObjectValue: &api.Value{
			Val: &api.Value_StrVal{StrVal: xid},
		},
	}}
	m.processNQuad(nq)
	return uid
}

func parseNQuad(line string) (gql.NQuad, error) {
	nq, err := rdf.Parse(line)
	if err != nil {
		return gql.NQuad{}, err
	}
	return gql.NQuad{NQuad: &nq}, nil
}

func (m *mapper) createPredicatePosting(predicate string) *intern.Posting {
	fp := farm.Fingerprint64([]byte(predicate))
	return &intern.Posting{
		Uid:         fp,
		Value:       []byte(predicate),
		ValType:     intern.Posting_DEFAULT,
		PostingType: intern.Posting_VALUE,
	}
}

func (m *mapper) createPostings(nq gql.NQuad,
	de *intern.DirectedEdge) (*intern.Posting, *intern.Posting) {

	m.schema.validateType(de, nq.ObjectValue == nil)

	p := posting.NewPosting(de)
	sch := m.schema.getSchema(nq.GetPredicate())
	if nq.GetObjectValue() != nil {
		if lang := de.GetLang(); len(lang) > 0 {
			p.Uid = farm.Fingerprint64([]byte(lang))
		} else if sch.List {
			p.Uid = farm.Fingerprint64(de.Value)
		} else {
			p.Uid = math.MaxUint64
		}
	}
	p.Facets = nq.Facets

	// Early exit for no reverse edge.
	if sch.GetDirective() != intern.SchemaUpdate_REVERSE {
		return p, nil
	}

	// Reverse predicate
	x.AssertTruef(nq.GetObjectValue() == nil, "only has reverse schema if object is UID")
	de.Entity, de.ValueId = de.ValueId, de.Entity
	m.schema.validateType(de, true)
	rp := posting.NewPosting(de)

	de.Entity, de.ValueId = de.ValueId, de.Entity // de reused so swap back.

	return p, rp
}

func (m *mapper) addIndexMapEntries(nq gql.NQuad, de *intern.DirectedEdge) {
	if nq.GetObjectValue() == nil {
		return // Cannot index UIDs
	}

	sch := m.schema.getSchema(nq.GetPredicate())
	for _, tokerName := range sch.GetTokenizer() {
		// Find tokeniser.
		toker, ok := tok.GetTokenizer(tokerName)
		if !ok {
			log.Fatalf("unknown tokenizer %q", tokerName)
		}

		// Create storage value.
		storageVal := types.Val{
			Tid:   types.TypeID(de.GetValueType()),
			Value: de.GetValue(),
		}

		// Convert from storage type to schema type.
		schemaVal, err := types.Convert(storageVal, types.TypeID(sch.GetValueType()))
		// Shouldn't error, since we've already checked for convertibility when
		// doing edge postings. So okay to be fatal.
		x.Check(err)

		// Extract tokens.
		toks, err := tok.BuildTokens(schemaVal.Value, toker)
		x.Check(err)

		// Store index posting.
		for _, t := range toks {
			m.addMapEntry(
				x.IndexKey(nq.Predicate, t),
				&intern.Posting{
					Uid:         de.GetEntity(),
					PostingType: intern.Posting_REF,
				},
				m.state.shards.shardFor(nq.Predicate),
			)
		}
	}
}
