// Copyright (c) 2020-2024 Blockwatch Data Inc.
// Author: alex@blockwatch.cc

package index

import (
	"context"
	"fmt"
	"io"

	"blockwatch.cc/packdb/pack"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/mavryk-network/mvgo/micheline"
	"github.com/mavryk-network/mvindex/etl/model"
	"github.com/mavryk-network/mvindex/etl/task"
)

const BigmapIndexKey = "bigmap"

type BigmapIndex struct {
	db         *pack.DB
	tables     map[string]*pack.Table
	allocCache *lru.Cache[int64, *model.BigmapAlloc] // cache bigmap allocs (for fast type access)
}

var _ model.BlockIndexer = (*BigmapIndex)(nil)

func NewBigmapIndex() *BigmapIndex {
	ac, _ := lru.New[int64, *model.BigmapAlloc](1 << 15) // 32k
	return &BigmapIndex{
		tables:     make(map[string]*pack.Table),
		allocCache: ac,
	}
}

func (idx *BigmapIndex) DB() *pack.DB {
	return idx.db
}

func (idx *BigmapIndex) Tables() []*pack.Table {
	t := []*pack.Table{}
	for _, v := range idx.tables {
		t = append(t, v)
	}
	return t
}

func (idx *BigmapIndex) Key() string {
	return BigmapIndexKey
}

func (idx *BigmapIndex) Name() string {
	return BigmapIndexKey + " index"
}

func (idx *BigmapIndex) Create(path, label string, opts interface{}) error {
	db, err := pack.CreateDatabase(path, idx.Key(), label, opts)
	if err != nil {
		return fmt.Errorf("creating database: %v", err)
	}
	defer db.Close()

	for _, m := range []model.Model{
		model.BigmapAlloc{},
		model.BigmapUpdate{},
		model.BigmapValue{},
	} {
		key := m.TableKey()
		fields, err := pack.Fields(m)
		if err != nil {
			return fmt.Errorf("reading fields for table %q from type %T: %v", key, m, err)
		}
		opts := m.TableOpts().Merge(model.ReadConfigOpts(key))
		_, err = db.CreateTableIfNotExists(key, fields, opts)
		if err != nil {
			return err
		}
	}
	return nil
}

func (idx *BigmapIndex) Init(path, label string, opts interface{}) error {
	db, err := pack.OpenDatabase(path, idx.Key(), label, opts)
	if err != nil {
		return err
	}
	idx.db = db

	for _, m := range []model.Model{
		model.BigmapAlloc{},
		model.BigmapUpdate{},
		model.BigmapValue{},
	} {
		key := m.TableKey()
		t, err := idx.db.Table(key, m.TableOpts().Merge(model.ReadConfigOpts(key)))
		if err != nil {
			idx.Close()
			return err
		}
		idx.tables[key] = t
	}
	return nil
}

func (idx *BigmapIndex) FinalizeSync(_ context.Context) error {
	return nil
}

func (idx *BigmapIndex) Close() error {
	for n, v := range idx.tables {
		if err := v.Close(); err != nil {
			log.Errorf("Closing %s table: %s", v.Name(), err)
		}
		delete(idx.tables, n)
	}
	if idx.db != nil {
		if err := idx.db.Close(); err != nil {
			return err
		}
		idx.db = nil
	}
	return nil
}

type InMemoryBigmap struct {
	Alloc   *model.BigmapAlloc
	Updates []*model.BigmapUpdate
	Live    []*model.BigmapValue
}

func NewInMemoryBigmap(alloc *model.BigmapAlloc) *InMemoryBigmap {
	return &InMemoryBigmap{
		Alloc:   alloc,
		Updates: make([]*model.BigmapUpdate, 0),
		Live:    make([]*model.BigmapValue, 0),
	}
}

func (idx *BigmapIndex) loadAlloc(ctx context.Context, id int64) (*model.BigmapAlloc, error) {
	alloc, ok := idx.allocCache.Get(id)
	if ok {
		return alloc, nil
	}
	alloc = &model.BigmapAlloc{}
	err := pack.NewQuery("etl.find_alloc").
		WithTable(idx.tables[model.BigmapAllocTableKey]).
		AndEqual("bigmap_id", id).
		Execute(ctx, alloc)
	if err != nil {
		return nil, fmt.Errorf("etl.bigmap.alloc decode: %v", err)
	}
	idx.allocCache.Add(id, alloc)
	return alloc, nil
}

// assumes op ids are already set (must run after OpIndex)
func (idx *BigmapIndex) ConnectBlock(ctx context.Context, block *model.Block, builder model.BlockBuilder) error {
	allocTable := idx.tables[model.BigmapAllocTableKey]
	updateTable := idx.tables[model.BigmapUpdateTableKey]
	valueTable := idx.tables[model.BigmapValueTableKey]

	tmp := make(map[int64]*InMemoryBigmap)
	for _, op := range block.Ops {
		// reset temp bigmap after a batch of internal ops has been processed
		if !op.IsInternal && len(tmp) > 0 {
			for k := range tmp {
				delete(tmp, k)
			}
		}

		// skip non-bigmap ops
		if len(op.BigmapEvents) == 0 || !op.IsSuccess {
			continue
		}

		// process bigmapdiffs
		for _, diff := range op.BigmapEvents {
			switch diff.Action {
			case micheline.DiffActionAlloc:
				// post Jakarta v013, bitmap allocs no longer contain type annotations
				// so we must lookup the correct bigmap type from script (exclude copies)
				if block.Params.Version >= 13 && diff.Id > 0 {
					script, err := op.Contract.LoadScript()
					if err != nil {
						return fmt.Errorf("etl.bigmap_alloc.load_type: %v", err)
					}
					var matchFound bool
					// compare the allocated bigmap type with annotated type in storage
					// using converted typedef to match comb and tree type pairs
					kt := micheline.NewType(diff.KeyType).Typedef("").Unfold()
					vt := micheline.NewType(diff.ValueType).Typedef("").Unfold()
					for _, btyp := range script.BigmapTypes() {
						if !btyp.Left().Typedef("").Unfold().Equal(kt) {
							continue
						}
						if !btyp.Right().Typedef("").Unfold().Equal(vt) {
							continue
						}
						// overwrite type in bigmap diff with annotated type from script
						diff.KeyType = btyp.Left().Prim.Clone()
						diff.ValueType = btyp.Right().Prim.Clone()
						matchFound = true
						break
					}
					if !matchFound {
						log.Errorf("No type match found for bigmap %d in %s for script %s",
							diff.Id, op.Hash, op.Contract)
						// } else {
						// 	log.Debugf("Bigmap %d type replaced from script %s", diff.Id, op.Contract)
					}
				}

				// insert immediately to allow sequence of updates
				// log.Debugf("Bigmap %d %s in block %d op %s", diff.Id, diff.Action, block.Height, op.Hash)
				if diff.Id < 0 {
					// alloc temp bigmap
					alloc := model.NewBigmapAlloc(op, diff)
					tmp[diff.Id] = NewInMemoryBigmap(alloc)
				} else {
					// alloc real bigmap
					alloc := model.NewBigmapAlloc(op, diff)
					if err := allocTable.Insert(ctx, alloc); err != nil {
						return fmt.Errorf("etl.bigmap_alloc.insert: %v", err)
					}
					idx.allocCache.Add(alloc.BigmapId, alloc)
					// log.Debugf("Bigmap type %d stored as id %d", alloc.BigmapId, alloc.RowId)

					// store as update
					if err := updateTable.Insert(ctx, alloc.ToUpdate(op)); err != nil {
						return fmt.Errorf("etl.bigmap_alloc.insert: %v", err)
					}
				}

			case micheline.DiffActionCopy:
				// copy the alloc
				// copy all live keys
				// generate updates for inserting all live keys
				var (
					alloc   *model.BigmapAlloc
					updates = make([]*model.BigmapUpdate, 0)
					live    = make([]*model.BigmapValue, 0)
				)

				if diff.SourceId < 0 {
					// use temporary bigmap as source
					bm, ok := tmp[diff.SourceId]
					if !ok {
						return fmt.Errorf("etl.bigmap.copy: missing temporary bigmap %d", diff.SourceId)
					}

					// create new alloc
					alloc = model.CopyBigmapAlloc(bm.Alloc, op, diff.DestId)
					// log.Debugf("Bigmap %s %d: copy alloc from temp map %d to %d",
					// 	diff.Action, diff.SourceId, bm.Alloc.BigmapId, diff.DestId)

					// add a copy update
					updates = append(updates, alloc.ToUpdateCopy(op, diff.SourceId))

					// copy live keys and create updates
					for _, v := range bm.Live {
						copied := model.CopyBigmapValue(v, diff.DestId, op.Height)
						live = append(live, copied)
						updates = append(updates, copied.ToUpdateCopy(op))
						// log.Debugf("Bigmap %s %d: copy live key %x from temp map %d",
						// 	diff.Action, diff.SourceId, v.Key, bm.Alloc.BigmapId)
					}
					alloc.NKeys = int64(len(live))
					alloc.NUpdates = int64(len(live))

				} else {
					// find the source alloc
					srcAlloc, err := idx.loadAlloc(ctx, diff.SourceId)
					if err != nil {
						return fmt.Errorf("etl.bigmap.copy: %v", err)
					}
					alloc = model.CopyBigmapAlloc(srcAlloc, op, diff.DestId)

					// add a copy update
					updates = append(updates, alloc.ToUpdateCopy(op, diff.SourceId))
					// log.Debugf("Bigmap %s %d: copy alloc from map %d to %d",
					// 	diff.Action, diff.SourceId, srcAlloc.BigmapId, diff.DestId)

					// load all currently live bigmap entries from source
					err = pack.NewQuery("etl.copy").
						WithTable(valueTable).
						AndEqual("bigmap_id", diff.SourceId).
						Stream(ctx, func(r pack.Row) error {
							source := &model.BigmapValue{}
							if err := r.Decode(source); err != nil {
								return err
							}
							copied := model.CopyBigmapValue(source, diff.DestId, op.Height)
							live = append(live, copied)
							updates = append(updates, copied.ToUpdateCopy(op))
							return nil
						})
					if err != nil {
						return fmt.Errorf("etl.bigmap.copy: %v", err)
					}
					alloc.NKeys = int64(len(live))
					alloc.NUpdates = int64(len(live))
				}

				if diff.DestId < 0 {
					// keep temp bigmaps around
					bm := NewInMemoryBigmap(alloc)
					bm.Updates = updates
					bm.Live = live
					tmp[diff.DestId] = bm
					// log.Debugf("Bigmap %s %d: store new temp map %d with %d live keys",
					// 	diff.Action, diff.SourceId, bm.Alloc.BigmapId, len(live))
				} else {
					// store copied data
					if err := allocTable.Insert(ctx, alloc); err != nil {
						return fmt.Errorf("etl.bigmap.insert: %v", err)
					}
					idx.allocCache.Add(alloc.BigmapId, alloc)
					ins := make([]pack.Item, len(live))
					for i, v := range live {
						ins[i] = v
					}
					if err := valueTable.Insert(ctx, ins); err != nil {
						return fmt.Errorf("etl.bigmap.insert: %v", err)
					}
					ins = ins[:0]
					for _, v := range updates {
						ins = append(ins, v)
					}
					if err := updateTable.Insert(ctx, ins); err != nil {
						return fmt.Errorf("etl.bigmap.insert: %v", err)
					}
					// log.Debugf("Bigmap %s %d: store new map %d with %d live keys",
					// 	diff.Action, diff.SourceId, alloc.BigmapId, len(live))
				}

			case micheline.DiffActionRemove:
				// full bigmap removal
				if !diff.KeyHash.IsValid() {
					if diff.Id < 0 {
						// clear temp bigmap
						bm := tmp[diff.Id]
						delete(tmp, diff.Id)
						if bm.Alloc != nil {
							if err := updateTable.Insert(ctx, bm.Alloc.ToRemove(op)); err != nil {
								return fmt.Errorf("etl.bigmap.empty: %v", err)
							}
						}

						// done, next bigmap diff
						continue
					}

					// for regular bigmaps, update alloc
					alloc, err := idx.loadAlloc(ctx, diff.Id)
					if err != nil {
						return fmt.Errorf("etl.bigmap.empty: %v", err)
					}

					// list all live keys and schedule for deletion
					ids := make([]uint64, 0, 1024)
					updates := make([]pack.Item, 0, 1024)
					err = pack.NewQuery("etl.empty").
						WithTable(valueTable).
						AndEqual("bigmap_id", diff.Id).
						Stream(ctx, func(r pack.Row) error {
							source := &model.BigmapValue{}
							if err := r.Decode(source); err != nil {
								return err
							}
							ids = append(ids, source.RowId)
							updates = append(updates, source.ToUpdateRemove(op))
							return nil
						})
					if err != nil {
						return fmt.Errorf("etl.bigmap.empty decode: %v", err)
					}
					alloc.NKeys = 0
					alloc.NUpdates += int64(len(updates))
					alloc.Updated = op.Height
					alloc.Deleted = op.Height

					// add bigmap remove at end
					if err := updateTable.Insert(ctx, alloc.ToRemove(op)); err != nil {
						return fmt.Errorf("etl.bigmap.empty: %v", err)
					}

					if err := allocTable.Update(ctx, alloc); err != nil {
						return fmt.Errorf("etl.bigmap.empty: %v", err)
					}
					if err := updateTable.Insert(ctx, updates); err != nil {
						return fmt.Errorf("etl.bigmap.empty: %v", err)
					}
					if err := valueTable.DeleteIds(ctx, ids); err != nil {
						return fmt.Errorf("etl.bigmap.empty: %v", err)
					}

					// done, next bigmap diff
					continue
				}

				// single key removal from temp bigmap
				if diff.Id < 0 {
					// use temporary bigmap as source
					bm, ok := tmp[diff.Id]
					if !ok {
						return fmt.Errorf("etl.bigmap.remove: missing temporary bigmap %d", diff.Id)
					}

					// find the key to remove and remove from live & update lists
					var pos int = -1
					for i, v := range bm.Live {
						if !v.GetKeyHash().Equal(diff.KeyHash) {
							continue
						}
						pos = i
						break
					}
					if pos > -1 {
						// add remove action
						if err := updateTable.Insert(ctx, bm.Live[pos].ToUpdateRemove(op)); err != nil {
							return fmt.Errorf("etl.bigmap.empty: %v", err)
						}
						bm.Alloc.NKeys--
						bm.Live = append(bm.Live[:pos], bm.Live[pos+1:]...)
						// log.Debugf("Bigmap %s %d: remove single key from temp map %d with %d live keys",
						// 	diff.Action, diff.Id, bm.Alloc.BigmapId, len(bm.Live))
					}
					pos = -1
					for i, v := range bm.Updates {
						if !v.GetKeyHash().Equal(diff.KeyHash) {
							continue
						}
						pos = i
						break
					}
					if pos > -1 {
						bm.Updates = append(bm.Updates[:pos], bm.Updates[pos+1:]...)
					}

					// done, next bigmap diff
					continue
				}

				// single key removal from regular bigmap
				alloc, err := idx.loadAlloc(ctx, diff.Id)
				if err != nil {
					return fmt.Errorf("etl.bigmap.remove: %v", err)
				}

				// find the previous entry, key should exist
				var prev *model.BigmapValue
				err = pack.NewQuery("etl.remove").
					WithTable(valueTable).
					AndEqual("bigmap_id", diff.Id).
					AndEqual("key_id", model.GetKeyId(diff.Id, diff.KeyHash)).
					WithDesc().
					Stream(ctx, func(r pack.Row) error {
						source := &model.BigmapValue{}
						if err := r.Decode(source); err != nil {
							return err
						}
						// additional check for hash collision safety
						if source.BigmapId == diff.Id && source.GetKeyHash().Equal(diff.KeyHash) {
							prev = source
							return io.EOF
						}
						return nil
					})
				if err != nil && err != io.EOF {
					return fmt.Errorf("etl.bigmap.remove decode: %v", err)
				}

				if prev != nil {
					// log.Debugf("Bigmap %s %d: remove single key from map %d with %d live keys",
					// 	diff.Action, diff.Id, alloc.BigmapId, alloc.NKeys)
					if err := valueTable.DeleteIds(ctx, []uint64{prev.RowId}); err != nil {
						return fmt.Errorf("etl.bigmap.remove: %v", err)
					}
					alloc.NKeys--
					// } else {
					// 	// double remove is possible, actually its double update
					// 	// with empty value (which we translate into remove)
					// 	log.Debugf("bigmap: remove on non-existing key %d %s in %s", diff.Id, diff.KeyHash, op.Hash)
				}
				alloc.Updated = op.Height
				alloc.NUpdates++
				if err := updateTable.Insert(ctx, model.NewBigmapUpdate(op, diff)); err != nil {
					return fmt.Errorf("etl.bigmap.remove: %v", err)
				}
				if err := allocTable.Update(ctx, alloc); err != nil {
					return fmt.Errorf("etl.bigmap.remove: %v", err)
				}

			case micheline.DiffActionUpdate:
				// update on temp bigmap
				if diff.Id < 0 {
					// use temporary bigmap as source
					bm, ok := tmp[diff.Id]
					if !ok {
						return fmt.Errorf("etl.bigmap.update: missing temporary bigmap %d", diff.Id)
					}

					// find the key to update in live list
					// create live item if none exists
					// always create update item
					var pos int = -1
					for i, v := range bm.Live {
						if !v.GetKeyHash().Equal(diff.KeyHash) {
							continue
						}
						pos = i
						break
					}
					if pos < 0 {
						// add
						bm.Alloc.NKeys++
						bm.Alloc.NUpdates++
						bm.Live = append(bm.Live, model.NewBigmapValue(diff, op.Height))
						bm.Updates = append(bm.Updates, model.NewBigmapUpdate(op, diff))
						// log.Debugf("Bigmap %s %d: add key to temp map %d with %d live keys",
						// 	diff.Action, diff.Id, bm.Alloc.BigmapId, len(bm.Live))
					} else {
						// replace
						bm.Alloc.NUpdates++
						bm.Live[pos] = model.NewBigmapValue(diff, op.Height)
						bm.Updates = append(bm.Updates, model.NewBigmapUpdate(op, diff))
						// log.Debugf("Bigmap %s %d: replace key in temp map %d with %d live keys",
						// 	diff.Action, diff.Id, bm.Alloc.BigmapId, len(bm.Live))
					}

					// insert to update table
					if err := updateTable.Insert(ctx, bm.Updates[len(bm.Updates)-1]); err != nil {
						return fmt.Errorf("etl.bigmap.update: %v", err)
					}

					// done, next bigmap diff
					continue
				}

				// regular bigmaps
				alloc, err := idx.loadAlloc(ctx, diff.Id)
				if err != nil {
					return fmt.Errorf("etl.bigmap.update: load alloc: %v", err)
				}

				// find the previous entry, key should exist
				var prev *model.BigmapValue
				err = pack.NewQuery("etl.update").
					WithTable(valueTable).
					AndEqual("bigmap_id", diff.Id).
					AndEqual("key_id", model.GetKeyId(diff.Id, diff.KeyHash)).
					WithDesc().
					Stream(ctx, func(r pack.Row) error {
						source := &model.BigmapValue{}
						if err := r.Decode(source); err != nil {
							return err
						}
						// additional check for hash collision safety
						if source.BigmapId == diff.Id && source.GetKeyHash().Equal(diff.KeyHash) {
							prev = source
							return io.EOF
						}
						return nil
					})
				if err != nil && err != io.EOF {
					return fmt.Errorf("etl.bigmap.update decode: %v", err)
				}

				live := model.NewBigmapValue(diff, op.Height)
				if prev != nil {
					// replace
					live.RowId = prev.RowId
					if err := valueTable.Update(ctx, live); err != nil {
						return fmt.Errorf("etl.bigmap.replace: %v", err)
					}
					// log.Debugf("Bigmap %s %d: replace key in map %d with %d live keys",
					// 	diff.Action, diff.Id, alloc.BigmapId, alloc.NKeys)
				} else {
					// add
					if err := valueTable.Insert(ctx, live); err != nil {
						return fmt.Errorf("etl.bigmap.insert: %v", err)
					}
					alloc.NKeys++
					// log.Debugf("Bigmap %s %d: add new key to map %d with %d live keys",
					// 	diff.Action, diff.Id, alloc.BigmapId, alloc.NKeys)
				}
				alloc.Updated = op.Height
				alloc.NUpdates++

				if err := updateTable.Insert(ctx, model.NewBigmapUpdate(op, diff)); err != nil {
					return fmt.Errorf("etl.bigmap.update: insert into %d: %v", alloc.BigmapId, err)
				}
				if err := allocTable.Update(ctx, alloc); err != nil {
					return fmt.Errorf("etl.bigmap.update: update alloc %d: %v -- diff=%#v", diff.Id, err, diff)
				}
			}
		}
	}

	return nil
}

func (idx *BigmapIndex) DisconnectBlock(ctx context.Context, block *model.Block, _ model.BlockBuilder) error {
	idx.allocCache.Purge()
	return idx.DeleteBlock(ctx, block.Height)
}

func (idx *BigmapIndex) DeleteBlock(ctx context.Context, height int64) error {
	allocTable := idx.tables[model.BigmapAllocTableKey]
	updateTable := idx.tables[model.BigmapUpdateTableKey]
	valueTable := idx.tables[model.BigmapValueTableKey]

	// reconstruct live keys by rolling back updates
	updates := make([]*model.BigmapUpdate, 0)
	err := pack.NewQuery("etl.delete_scan").
		WithTable(updateTable).
		WithDesc().
		AndEqual("height", height).
		Execute(ctx, &updates)
	if err != nil {
		return err
	}

	// load allocs for rollback along the way, update all at once at the end
	allocs := make(map[int64]*model.BigmapAlloc)

	// identify list of updated keys
	for _, v := range updates {
		hash := v.GetKeyHash()
		key := model.GetKeyId(v.BigmapId, hash)

		// load alloc first
		alloc, ok := allocs[v.BigmapId]
		if !ok {
			alloc, err = idx.loadAlloc(ctx, v.BigmapId)
			if err != nil {
				return fmt.Errorf("rollback: missing alloc for bigmap %d: %v", v.BigmapId, err)
			}
			alloc.Updated = 0
			allocs[v.BigmapId] = alloc
		}

		// find the previous live entry, may not exist, may be from same block(!)
		var (
			prev *model.BigmapUpdate
			live *model.BigmapValue
		)
		err = pack.NewQuery("etl.rollback").
			WithTable(updateTable).
			AndEqual("bigmap_id", v.BigmapId).
			AndEqual("key_id", key).
			AndLt("I", v.RowId).            // exclude self
			AndLte("height", height).       // limit search scope
			AndGte("height", alloc.Height). // limit search scope
			Stream(ctx, func(r pack.Row) error {
				source := &model.BigmapUpdate{}
				if err := r.Decode(source); err != nil {
					return err
				}
				// additional check for hash collision safety
				if source.GetKeyHash().Equal(hash) {
					prev = source
					return io.EOF
				}
				return nil
			})
		if err != nil && err != io.EOF {
			return fmt.Errorf("etl.bigmap.rollback decode: %v", err)
		}

		// rollback update
		switch v.Action {
		case micheline.DiffActionRemove:
			// sanity checks
			if prev == nil {
				log.Warnf("rollback: missing previous update for bigmap %d key %s", v.BigmapId, v.GetKeyHash())
				continue
			}
			if prev.Action != micheline.DiffActionUpdate {
				// just skip if this was a double remove
				alloc.NUpdates--
				log.Debugf("rollback: unexpected prev action %s update for bigmap %d key %s", prev.Action, v.BigmapId, v.GetKeyHash())
			} else {
				// this was a remove after update, insert previous live key
				live = prev.ToKV()
				alloc.NKeys++
				alloc.NUpdates--
				if err := valueTable.Insert(ctx, live); err != nil {
					return fmt.Errorf("etl.bigmap.rollback insert live key: %v", err)
				}
			}
			// beware of same-block updates when resetting alloc update
			if prev.Height < height {
				alloc.Updated = max(alloc.Updated, prev.Height)
			}

		case micheline.DiffActionUpdate, micheline.DiffActionCopy:
			// load current live key, may not exist
			err = pack.NewQuery("etl.rollback").
				WithTable(valueTable).
				AndEqual("bigmap_id", v.BigmapId).
				AndEqual("key_id", key).
				Stream(ctx, func(r pack.Row) error {
					source := &model.BigmapValue{}
					if err := r.Decode(source); err != nil {
						return err
					}
					// additional check for hash collision safety
					if source.GetKeyHash().Equal(hash) {
						live = source
						return io.EOF
					}
					return nil
				})
			if err != nil && err != io.EOF {
				return fmt.Errorf("etl.bigmap.rollback decode: %v", err)
			}
			if prev == nil {
				// sanity check
				if live == nil {
					log.Warnf("rollback: missing live key in bigmap %d key %s", v.BigmapId, v.GetKeyHash())
					continue
				}

				// this was a first-time insert, delete current live key
				if err := valueTable.DeleteIds(ctx, []uint64{live.RowId}); err != nil {
					return fmt.Errorf("etl.bigmap.rollback delete live key: %v", err)
				}
				alloc.NKeys--
				alloc.NUpdates--

				// NOTE: we don't know previous update height here, but its ok
				// since alloc.Updated field is not relevant for correctness

			} else {
				if prev.Action == micheline.DiffActionRemove {
					// this was an insert after remove, remove current live key
					if err := valueTable.DeleteIds(ctx, []uint64{live.RowId}); err != nil {
						return fmt.Errorf("etl.bigmap.rollback delete live key: %v", err)
					}
					alloc.NKeys--
					alloc.NUpdates--

				} else {
					// sanity check
					if live == nil {
						return fmt.Errorf("rollback: missing live key in bigmap %d key %s", v.BigmapId, v.GetKeyHash())
					}

					// this was an update after update, replace current live key
					lastLive := prev.ToKV()
					lastLive.RowId = live.RowId
					if err := valueTable.Update(ctx, lastLive); err != nil {
						return fmt.Errorf("etl.bigmap.rollback replace live key: %v", err)
					}
					alloc.NUpdates--
				}

				// beware of same-block updates when resetting alloc update
				if prev.Height < height {
					alloc.Updated = max(alloc.Updated, prev.Height)
				}
			}
		}
	}

	// delete all updates at height
	_, err = pack.NewQuery("etl.delete").
		WithTable(updateTable).
		AndEqual("height", height).
		Delete(ctx)
	if err != nil {
		return err
	}

	// update rolled back allocs
	upd := make([]pack.Item, 0)
	for _, v := range allocs {
		if v.Height != height {
			continue
		}
		upd = append(upd, v)
	}
	if err := allocTable.Update(ctx, upd); err != nil {
		return err
	}

	// delete all allocs from this block
	_, err = pack.NewQuery("etl.delete").
		WithTable(allocTable).
		AndEqual("h", height). // alloc height
		Delete(ctx)
	if err != nil {
		return err
	}

	return nil
}

func (idx *BigmapIndex) DeleteCycle(ctx context.Context, cycle int64) error {
	return nil
}

func (idx *BigmapIndex) Flush(ctx context.Context) error {
	for n, v := range idx.tables {
		if err := v.Flush(ctx); err != nil {
			log.Errorf("Flushing %s table: %v", n, err)
		}
	}
	return nil
}

func (idx *BigmapIndex) OnTaskComplete(_ context.Context, _ *task.TaskResult) error {
	// unused
	return nil
}
