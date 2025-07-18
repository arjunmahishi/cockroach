// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package vecstore

import (
	"context"
	"slices"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catid"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/vecindex/cspann"
	"github.com/cockroachdb/cockroach/pkg/sql/vecindex/cspann/quantize"
	"github.com/cockroachdb/cockroach/pkg/sql/vecindex/vecencoding"
	"github.com/cockroachdb/cockroach/pkg/sql/vecindex/vecstore/vecstorepb"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/unique"
	"github.com/cockroachdb/cockroach/pkg/util/vector"
	"github.com/cockroachdb/errors"
)

var moveFailedErr = errors.New("TryMoveVector failed to find source vector")

// Store implements the cspann.Store interface for KV backed vector indices.
type Store struct {
	db descs.DB // Used to generate new partition IDs
	kv *kv.DB

	// readOnly is true if the store does not accept writes.
	readOnly bool

	codec   keys.SQLCodec
	tableID catid.DescID
	indexID catid.IndexID

	// The root partition always uses the UnQuantizer while other partitions may
	// use any quantizer.
	rootQuantizer quantize.Quantizer
	quantizer     quantize.Quantizer

	// minConsistency can override default INCONSISTENCY usage when estimating
	// the size of a partition. This is used for testing.
	minConsistency kvpb.ReadConsistencyType

	prefix   roachpb.Key // KV prefix for the vector index.
	emptyVec vector.T    // A zero-valued vector, used when root centroid does not exist.

	// These are set by NewWithLeasedDesc and should only be used for testing.
	TestingTableDesc catalog.TableDescriptor
}

var _ cspann.Store = (*Store)(nil)

// NewWithLeasedDesc creates a Store for an index on the provided table descriptor
// using the provided index descriptor. This is used in unit tests where full
// vector index creation capabilities aren't necessarily available. This creation
// method doesn't support external row data.
func NewWithLeasedDesc(
	ctx context.Context,
	db descs.DB,
	quantizer quantize.Quantizer,
	codec keys.SQLCodec,
	tableDesc catalog.TableDescriptor,
	indexID catid.IndexID,
) (ps *Store, err error) {
	ps = &Store{
		db:               db,
		kv:               db.KV(),
		codec:            codec,
		tableID:          tableDesc.GetID(),
		indexID:          indexID,
		rootQuantizer:    quantize.NewUnQuantizer(quantizer.GetDims(), quantizer.GetDistanceMetric()),
		quantizer:        quantizer,
		minConsistency:   kvpb.INCONSISTENT,
		emptyVec:         make(vector.T, quantizer.GetDims()),
		TestingTableDesc: tableDesc,
	}
	ps.prefix = rowenc.MakeIndexKeyPrefix(codec, tableDesc.GetID(), indexID)

	return ps, nil
}

// New creates a cspann.Store interface backed by the KV for a single vector
// index.
func New(
	ctx context.Context,
	db descs.DB,
	quantizer quantize.Quantizer,
	defaultCodec keys.SQLCodec,
	tableID catid.DescID,
	indexID catid.IndexID,
) (ps *Store, err error) {
	ps = &Store{
		db:             db,
		kv:             db.KV(),
		codec:          defaultCodec,
		tableID:        tableID,
		indexID:        indexID,
		rootQuantizer:  quantize.NewUnQuantizer(quantizer.GetDims(), quantizer.GetDistanceMetric()),
		quantizer:      quantizer,
		minConsistency: kvpb.INCONSISTENT,
		emptyVec:       make(vector.T, quantizer.GetDims()),
	}

	err = db.DescsTxn(ctx, func(ctx context.Context, txn descs.Txn) error {
		tableDesc, err := txn.Descriptors().ByIDWithLeased(txn.KV()).Get().Table(ctx, tableID)
		if err != nil {
			return err
		}

		if ext := tableDesc.ExternalRowData(); ext != nil {
			// The table is external, so use the external codec and table ID. Also set
			// the index to read-only.
			log.VInfof(ctx, 2,
				"table %d is external, using read-only mode for vector index %d",
				tableDesc.GetID(), indexID,
			)
			ps.readOnly = true
			ps.codec = keys.MakeSQLCodec(ext.TenantID)
			ps.tableID = ext.TableID
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	ps.prefix = rowenc.MakeIndexKeyPrefix(ps.codec, ps.tableID, ps.indexID)

	return ps, nil
}

// SetConsistency sets the minimum consistency level to use when reading
// partitions. This is set to a higher level for deterministic tests.
func (s *Store) SetMinimumConsistency(consistency kvpb.ReadConsistencyType) {
	s.minConsistency = consistency
}

// ReadOnly returns true if the store does not allow writes.
func (s *Store) ReadOnly() bool {
	return s.readOnly
}

// RunTransaction is part of the cspann.Store interface. It runs a function in
// the context of a transaction.
func (s *Store) RunTransaction(ctx context.Context, fn func(txn cspann.Txn) error) (err error) {
	return s.db.DescsTxn(ctx, func(ctx context.Context, txn descs.Txn) error {
		var tableDesc catalog.TableDescriptor
		if s.TestingTableDesc != nil {
			tableDesc = s.TestingTableDesc
		} else {
			tableDesc, err = txn.Descriptors().ByIDWithLeased(txn.KV()).Get().Table(ctx, s.tableID)
			if err != nil {
				return err
			}
		}

		indexDesc, err := catalog.MustFindIndexByID(tableDesc, s.indexID)
		if err != nil {
			return err
		}

		var fullVecFetchSpec vecstorepb.GetFullVectorsFetchSpec
		if err := InitGetFullVectorsFetchSpec(
			&fullVecFetchSpec,
			&eval.Context{Codec: s.codec},
			tableDesc,
			indexDesc,
			tableDesc.GetPrimaryIndex(),
		); err != nil {
			return err
		}

		var tx Txn
		tx.Init(&eval.Context{}, s, txn.KV(), &fullVecFetchSpec)

		err = fn(&tx)
		if err != nil {
			log.Errorf(ctx, "error in RunTransaction: %v", err)
			return err
		}

		return nil
	})
}

// MakePartitionKey is part of the cspann.Store interface. It allocates a new
// unique partition key.
func (s *Store) MakePartitionKey() cspann.PartitionKey {
	instanceID := s.kv.Context().NodeID.SQLInstanceID()
	return cspann.PartitionKey(unique.GenerateUniqueUnorderedID(unique.ProcessUniqueID(instanceID)))
}

// EstimatePartitionCount is part of the cspann.Store interface. It returns an
// estimate of the number of vectors in the given partition.
func (s *Store) EstimatePartitionCount(
	ctx context.Context, treeKey cspann.TreeKey, partitionKey cspann.PartitionKey,
) (int, error) {
	// Create a batch with INCONSISTENT read consistency to avoid updating the
	// timestamp cache or blocking on locks.
	// NOTE: In rare edge cases, INCONSISTENT scans can return results that are
	// arbitrarily old. However, there is a fixup processor on every node, so each
	// partition has its size checked multiple times across nodes. At least two
	// nodes in a cluster will have up-to-date results for any given partition, so
	// stale results are not a concern in practice. If we ever find evidence that
	// it is, we can fall back to a consistent scan if the inconsistent scan
	// returns results that are too old.
	b := s.kv.NewBatch()
	b.Header.ReadConsistency = s.minConsistency

	// Count the number of rows in the partition after the metadata row.
	metadataKey := vecencoding.EncodeMetadataKey(s.prefix, treeKey, partitionKey)
	startKey := vecencoding.EncodeStartVectorKey(metadataKey)
	endKey := vecencoding.EncodeEndVectorKey(metadataKey)
	b.Scan(startKey, endKey)

	// Execute the batch and count the rows in the response.
	if err := s.kv.Run(ctx, b); err != nil {
		return 0, errors.Wrap(err, "estimating partition count")
	}
	if err := b.Results[0].Err; err != nil {
		return 0, errors.Wrap(err, "extracting Scan rows for partition count")
	}
	return len(b.Results[0].Rows), nil
}

// MergeStats is part of the cspann.Store interface.
func (s *Store) MergeStats(ctx context.Context, stats *cspann.IndexStats, skipMerge bool) error {
	if !skipMerge && s.ReadOnly() {
		return errors.AssertionFailedf("cannot merge stats in read-only mode")
	}
	// TODO(mw5h): Implement MergeStats. We're not panicking here because some tested
	// functionality needs to call this function but does not depend on the results.
	return nil
}

// TryDeletePartition is part of the cspann.Store interface. It deletes an
// existing partition.
func (s *Store) TryDeletePartition(
	ctx context.Context, treeKey cspann.TreeKey, partitionKey cspann.PartitionKey,
) error {
	if s.ReadOnly() {
		return errors.AssertionFailedf("cannot delete partition in read-only mode")
	}
	// Delete the metadata key and all vector keys in the partition.
	b := s.kv.NewBatch()
	metadataKey := vecencoding.EncodeMetadataKey(s.prefix, treeKey, partitionKey)
	endKey := vecencoding.EncodeEndVectorKey(metadataKey)
	b.DelRange(metadataKey, endKey, true /* returnKeys */)
	if err := s.kv.Run(ctx, b); err != nil {
		return err
	}
	if len(b.Results[0].Keys) == 0 {
		// No metadata row existed, so partition must not exist.
		return cspann.ErrPartitionNotFound
	}
	return nil
}

// TryCreateEmptyPartition is part of the cspann.Store interface. It creates a
// new partition that contains no vectors.
func (s *Store) TryCreateEmptyPartition(
	ctx context.Context,
	treeKey cspann.TreeKey,
	partitionKey cspann.PartitionKey,
	metadata cspann.PartitionMetadata,
) error {
	if s.ReadOnly() {
		return errors.AssertionFailedf("cannot create partition in read-only mode")
	}
	meta := vecencoding.EncodeMetadataValue(metadata)
	metadataKey := vecencoding.EncodeMetadataKey(s.prefix, treeKey, partitionKey)
	if err := s.kv.CPut(ctx, metadataKey, meta, nil /* expValue */); err != nil {
		return remapConditionFailedError(err)
	}
	return nil
}

// TryGetPartition is part of the cspann.Store interface. It returns an existing
// partition, including both its metadata and data.
func (s *Store) TryGetPartition(
	ctx context.Context, treeKey cspann.TreeKey, partitionKey cspann.PartitionKey,
) (*cspann.Partition, error) {
	b := s.kv.NewBatch()
	metadataKey := vecencoding.EncodeMetadataKey(s.prefix, treeKey, partitionKey)
	startKey := vecencoding.EncodeStartVectorKey(metadataKey)
	endKey := vecencoding.EncodeEndVectorKey(metadataKey)
	b.Get(metadataKey)
	b.Scan(startKey, endKey)
	if err := s.kv.Run(ctx, b); err != nil {
		return nil, err
	}

	codec := makePartitionCodec(s.rootQuantizer, s.quantizer)
	partition, err := s.decodePartition(treeKey, partitionKey, &codec, &b.Results[0], &b.Results[1])
	if err != nil {
		return nil, err
	}
	return partition, nil
}

// TryGetPartitionMetadata is part of the cspann.Store interface. It returns the
// metadata for a batch of partitions.
func (s *Store) TryGetPartitionMetadata(
	ctx context.Context, treeKey cspann.TreeKey, toGet []cspann.PartitionMetadataToGet,
) error {
	// Construct a batch with one Get request per partition.
	b := s.kv.NewBatch()
	for i := range toGet {
		metadataKey := vecencoding.EncodeMetadataKey(s.prefix, treeKey, toGet[i].Key)
		b.Get(metadataKey)
	}

	// Run the batch and return results.
	var err error
	if err = s.kv.Run(ctx, b); err != nil {
		return errors.Wrapf(err, "getting partition metadata for %d partitions", len(toGet))
	}

	for i := range toGet {
		item := &toGet[i]
		item.Metadata, err = s.getMetadataFromKVResult(item.Key, &b.Results[i])

		// If partition is missing, just return Missing metadata.
		if err != nil {
			if !errors.Is(err, cspann.ErrPartitionNotFound) {
				return err
			}
		}
	}

	return nil
}

// TryUpdatePartitionMetadata is part of the cspann.Store interface. It updates
// the metadata for an existing partition.
func (s *Store) TryUpdatePartitionMetadata(
	ctx context.Context,
	treeKey cspann.TreeKey,
	partitionKey cspann.PartitionKey,
	metadata cspann.PartitionMetadata,
	expected cspann.PartitionMetadata,
) error {
	if s.ReadOnly() {
		return errors.AssertionFailedf("cannot update partition in read-only mode")
	}
	metadataKey := vecencoding.EncodeMetadataKey(s.prefix, treeKey, partitionKey)
	encodedMetadata := vecencoding.EncodeMetadataValue(metadata)
	encodedExpected := vecencoding.EncodeMetadataValue(expected)

	var roachval roachpb.Value
	roachval.SetBytes(encodedExpected)
	err := s.kv.CPut(ctx, metadataKey, encodedMetadata, roachval.TagAndDataBytes())
	if err != nil {
		return remapConditionFailedError(err)
	}
	return nil
}

// TryAddToPartition is part of the cspann.Store interface. It adds vectors to
// an existing partition.
func (s *Store) TryAddToPartition(
	ctx context.Context,
	treeKey cspann.TreeKey,
	partitionKey cspann.PartitionKey,
	vectors vector.Set,
	childKeys []cspann.ChildKey,
	valueBytes []cspann.ValueBytes,
	expected cspann.PartitionMetadata,
) (added bool, err error) {
	if s.ReadOnly() {
		return added, errors.AssertionFailedf("cannot add to partition in read-only mode")
	}
	return added, s.kv.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		// Acquire a shared lock on the partition, to ensure that another agent
		// doesn't modify it. Also, this will be used to verify expected metadata.
		b := txn.NewBatch()
		metadataKey := vecencoding.EncodeMetadataKey(s.prefix, treeKey, partitionKey)
		b.GetForShare(metadataKey, kvpb.BestEffort)
		if err = txn.Run(ctx, b); err != nil {
			return errors.Wrapf(err, "locking partition %d for add", partitionKey)
		}

		// Verify expected metadata.
		metadata, err := s.getMetadataFromKVResult(partitionKey, &b.Results[0])
		if err != nil {
			return err
		}
		if !metadata.Equal(&expected) {
			return cspann.NewConditionFailedError(metadata)
		}
		if !metadata.StateDetails.State.AllowAdd() {
			return errors.AssertionFailedf(
				"cannot add to partition in state %s that disallows adds", metadata.StateDetails.State)
		}

		// Do not add vectors that are found to already exist.
		var exclude cspann.ChildKeyDeDup
		exclude.Init(vectors.Count)

		// Cap the key so that appends allocate a new slice.
		vectorKey := vecencoding.EncodePrefixVectorKey(metadataKey, metadata.Level)
		vectorKey = slices.Clip(vectorKey)
		for {
			// Quantize the vectors and add them to the partition with CPut commands
			// that only take action if there is no value present yet.
			b = txn.NewBatch()
			codec := makePartitionCodec(s.rootQuantizer, s.quantizer)
			for i := range vectors.Count {
				if !exclude.TryAdd(childKeys[i]) {
					// Vector already exists in the partition, do not add.
					continue
				}
				added = true

				encodedValue, err := codec.EncodeVector(partitionKey, vectors.At(i), metadata.Centroid)
				if err != nil {
					return err
				}
				encodedValue = append(encodedValue, valueBytes[i]...)

				encodedKey := vecencoding.EncodeChildKey(vectorKey, childKeys[i])
				b.CPut(encodedKey, encodedValue, nil /* expValue */)
			}

			if err = txn.Run(ctx, b); err == nil {
				// The batch succeeded, so done.
				return nil
			}

			// If the batch failed due to a CPut failure, then retry, but with
			// any existing vectors excluded.
			var errConditionFailed *kvpb.ConditionFailedError
			if !errors.As(err, &errConditionFailed) {
				// This was a different error, so exit.
				return err
			}

			// Scan for existing vectors so they can be excluded from next attempt
			// to add.
			added = false
			exclude.Clear()
			b = txn.NewBatch()
			startKey := vecencoding.EncodeStartVectorKey(metadataKey)
			endKey := vecencoding.EncodeEndVectorKey(metadataKey)
			b.Scan(startKey, endKey)
			if err = txn.Run(ctx, b); err != nil {
				return errors.Wrapf(err, "scanning for existing vectors in partition %d", partitionKey)
			}
			for _, keyval := range b.Results[0].Rows {
				// Extract child key from the KV key.
				prefixLen := len(vectorKey)
				childKey, err := vecencoding.DecodeChildKey(keyval.Key[prefixLen:], metadata.Level)
				if err != nil {
					return errors.Wrapf(err, "decoding vector index key in partition %d: %+v",
						partitionKey, keyval.Key)
				}
				exclude.TryAdd(childKey)
			}
		}
	})
}

// TryRemoveFromPartition is part of the cspann.Store interface. It removes
// vectors from an existing partition.
func (s *Store) TryRemoveFromPartition(
	ctx context.Context,
	treeKey cspann.TreeKey,
	partitionKey cspann.PartitionKey,
	childKeys []cspann.ChildKey,
	expected cspann.PartitionMetadata,
) (removed bool, err error) {
	if s.ReadOnly() {
		return removed, errors.AssertionFailedf("cannot remove from partition in read-only mode")
	}
	return removed, s.kv.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		// Acquire a shared lock on the partition, to ensure that another agent
		// doesn't modify it. Also, this will be used to verify expected metadata.
		b := txn.NewBatch()
		metadataKey := vecencoding.EncodeMetadataKey(s.prefix, treeKey, partitionKey)
		b.GetForShare(metadataKey, kvpb.BestEffort)
		if err = txn.Run(ctx, b); err != nil {
			return errors.Wrapf(err, "locking partition %d for add", partitionKey)
		}

		// Verify expected metadata.
		metadata, err := s.getMetadataFromKVResult(partitionKey, &b.Results[0])
		if err != nil {
			return err
		}
		if !metadata.Equal(&expected) {
			return cspann.NewConditionFailedError(metadata)
		}

		// Cap the vector key so that appends allocate a new slice.
		vectorKey := vecencoding.EncodePrefixVectorKey(metadataKey, metadata.Level)
		vectorKey = slices.Clip(vectorKey)

		// Quantize the vectors and remove them from the partition with Del
		// commands.
		b = txn.NewBatch()
		for _, childKey := range childKeys {
			encodedKey := vecencoding.EncodeChildKey(vectorKey, childKey)
			b.Del(encodedKey)
		}

		if err = txn.CommitInBatch(ctx, b); err != nil {
			return err
		}

		for _, response := range b.RawResponse().Responses {
			del := response.GetDelete()
			if del != nil && del.FoundKey {
				removed = true
				break
			}
		}

		return nil
	})
}

// TryMoveVector is part of the cspann.Store interface. It moves a vector from
// one partition to another.
func (s *Store) TryMoveVector(
	ctx context.Context,
	treeKey cspann.TreeKey,
	sourcePartitionKey, targetPartitionKey cspann.PartitionKey,
	vec vector.T,
	childKey cspann.ChildKey,
	valueBytes cspann.ValueBytes,
	expected cspann.PartitionMetadata,
) (moved bool, err error) {
	if s.ReadOnly() {
		return moved, errors.AssertionFailedf("cannot add to partition in read-only mode")
	}

	if sourcePartitionKey == targetPartitionKey {
		// No-op move.
		return false, nil
	}

	err = s.kv.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		// Acquire a shared lock on the target partition, to ensure that another
		// agent doesn't modify it. Also, this will be used to verify expected
		// metadata.
		b := txn.NewBatch()
		targetMetadataKey := vecencoding.EncodeMetadataKey(s.prefix, treeKey, targetPartitionKey)
		b.GetForShare(targetMetadataKey, kvpb.BestEffort)
		if err = txn.Run(ctx, b); err != nil {
			return errors.Wrapf(err, "locking target partition %d for move from partition %d",
				targetPartitionKey, sourcePartitionKey)
		}

		// Verify expected target metadata.
		targetMetadata, err := s.getMetadataFromKVResult(targetPartitionKey, &b.Results[0])
		if err != nil {
			if errors.Is(err, cspann.ErrPartitionNotFound) {
				// Suppress partition not found error.
				return nil
			}
			return err
		}
		if !targetMetadata.Equal(&expected) {
			return cspann.NewConditionFailedError(targetMetadata)
		}

		// Cap the target key so that appends allocate a new slice.
		targetVectorKey := vecencoding.EncodePrefixVectorKey(targetMetadataKey, targetMetadata.Level)
		targetVectorKey = slices.Clip(targetVectorKey)

		// Quantize the vector and add it to the target partition with CPut command
		// that only takes action if there is no value present yet.
		b = txn.NewBatch()
		codec := makePartitionCodec(s.rootQuantizer, s.quantizer)
		encodedValue, err := codec.EncodeVector(targetPartitionKey, vec, targetMetadata.Centroid)
		if err != nil {
			return err
		}
		encodedValue = append(encodedValue, valueBytes...)
		encodedKey := vecencoding.EncodeChildKey(targetVectorKey, childKey)
		b.CPut(encodedKey, encodedValue, nil /* expValue */)

		// Cap the source key so that appends allocate a new slice.
		sourceMetadataKey := vecencoding.EncodeMetadataKey(s.prefix, treeKey, sourcePartitionKey)
		sourceVectorKey := vecencoding.EncodePrefixVectorKey(sourceMetadataKey, targetMetadata.Level)
		sourceVectorKey = slices.Clip(sourceVectorKey)

		// Remove the vector from the source partition.
		encodedKey = vecencoding.EncodeChildKey(sourceVectorKey, childKey)
		b.Del(encodedKey)

		if err = txn.Run(ctx, b); err != nil {
			var errConditionFailed *kvpb.ConditionFailedError
			if errors.As(err, &errConditionFailed) {
				// The vector already exists in the target partition.
				return nil
			}
			return errors.Wrapf(err, "adding vector to partition %d from partition %d",
				targetPartitionKey, sourcePartitionKey)
		}

		// If vector was not removed from the source partition, then abort the
		// transaction and return moved = false.
		del := b.RawResponse().Responses[1].GetDelete()
		if !del.FoundKey {
			return moveFailedErr
		}

		moved = true
		return nil
	})
	if err != nil {
		if errors.Is(err, moveFailedErr) {
			// Suppress moveFailedErr, since it only exists to trigger abort of the
			// transaction (and roll back the add).
			err = nil
		}
		return false, err
	}
	return moved, nil
}

// TryClearPartition is part of the cspann.Store interface. It removes vectors
// from an existing partition.
func (s *Store) TryClearPartition(
	ctx context.Context,
	treeKey cspann.TreeKey,
	partitionKey cspann.PartitionKey,
	expected cspann.PartitionMetadata,
) (count int, err error) {
	if s.ReadOnly() {
		return count, errors.AssertionFailedf("cannot clear partition in read-only mode")
	}
	return count, s.kv.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		// Acquire a shared lock on the partition, to ensure that another agent
		// doesn't modify it. Also, this will be used to verify expected metadata.
		b := txn.NewBatch()
		metadataKey := vecencoding.EncodeMetadataKey(s.prefix, treeKey, partitionKey)
		b.GetForShare(metadataKey, kvpb.BestEffort)
		if err = txn.Run(ctx, b); err != nil {
			return errors.Wrapf(err, "locking partition %d for add", partitionKey)
		}

		// Verify expected metadata.
		metadata, err := s.getMetadataFromKVResult(partitionKey, &b.Results[0])
		if err != nil {
			return err
		}
		if !metadata.Equal(&expected) {
			return cspann.NewConditionFailedError(metadata)
		}
		if metadata.StateDetails.State.AllowAdd() {
			return errors.AssertionFailedf(
				"cannot clear partition in state %s that allows adds", metadata.StateDetails.State)
		}

		// Clear all vectors in the partition using DelRange.
		b = txn.NewBatch()
		startKey := vecencoding.EncodeStartVectorKey(metadataKey)
		endKey := vecencoding.EncodeEndVectorKey(metadataKey)
		b.DelRange(startKey, endKey, true /* returnKeys */)
		if err = txn.CommitInBatch(ctx, b); err != nil {
			return err
		}

		count = len(b.Results[0].Keys)
		return nil
	})
}

// getMetadataFromKVResult returns the partition metadata row from the KV
// result, returning the partition's K-means tree level and centroid.
func (s *Store) getMetadataFromKVResult(
	partitionKey cspann.PartitionKey, result *kv.Result,
) (cspann.PartitionMetadata, error) {
	if result.Err != nil {
		return cspann.PartitionMetadata{}, result.Err
	}

	// If the value of the first result row is nil and this is a root partition,
	// then it must be a root partition without a metadata record (a nil result
	// happens when Get is used to fetch the metadata row).
	value := result.Rows[0].ValueBytes()
	if value == nil {
		if partitionKey != cspann.RootKey {
			return cspann.PartitionMetadata{}, cspann.ErrPartitionNotFound
		}

		// Construct synthetic metadata.
		return cspann.MakeReadyPartitionMetadata(cspann.LeafLevel, s.emptyVec), nil
	}

	return vecencoding.DecodeMetadataValue(value)
}

// decodePartition decodes the metadata and data KV results into an ephemeral
// partition. This partition will become invalid when the codec is next reset,
// so it needs to be cloned if it will be used outside of the store.
func (s *Store) decodePartition(
	treeKey cspann.TreeKey,
	partitionKey cspann.PartitionKey,
	codec *partitionCodec,
	metaResult, dataResult *kv.Result,
) (*cspann.Partition, error) {
	metadata, err := s.getMetadataFromKVResult(partitionKey, metaResult)
	if err != nil {
		return nil, err
	}
	if dataResult.Err != nil {
		return nil, dataResult.Err
	}
	vectorEntries := dataResult.Rows

	// Initialize the partition codec.
	// NOTE: This reuses the memory returned by the last call to decodePartition.
	codec.InitForDecoding(partitionKey, metadata, len(vectorEntries))

	// Determine the length of the prefix of vector data records.
	metadataKey := vecencoding.EncodeMetadataKey(s.prefix, treeKey, partitionKey)
	prefixLen := vecencoding.EncodedPrefixVectorKeyLen(metadataKey, metadata.Level)
	for _, entry := range vectorEntries {
		err = codec.DecodePartitionData(entry.Key[prefixLen:], entry.ValueBytes())
		if err != nil {
			return nil, err
		}
	}

	return codec.GetPartition(), nil
}

// remapConditionFailedError checks if the provided error is
// kvpb.ConditionFiledError. If so, it translates it to a corresponding
// cspann.ConditionFailedError by deserializing the partition metadata. If the
// record does not exist, it returns ErrPartitionNotFound.
func remapConditionFailedError(err error) error {
	var errConditionFailed *kvpb.ConditionFailedError
	if errors.As(err, &errConditionFailed) {
		if errConditionFailed.ActualValue == nil {
			// Metadata record does not exist.
			return cspann.ErrPartitionNotFound
		}

		encodedMetadata, err := errConditionFailed.ActualValue.GetBytes()
		if err != nil {
			return errors.NewAssertionErrorWithWrappedErrf(err,
				"partition metadata value should always be bytes")
		}
		actualMetadata, err := vecencoding.DecodeMetadataValue(encodedMetadata)
		if err != nil {
			return errors.NewAssertionErrorWithWrappedErrf(err,
				"cannot decode partition metadata: %v", encodedMetadata)
		}
		return cspann.NewConditionFailedError(actualMetadata)
	}
	return err
}
