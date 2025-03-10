// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sql

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/errors"
)

type sequenceSelectNode struct {
	zeroInputPlanNode
	optColumnsSlot

	desc catalog.TableDescriptor

	val  int64
	done bool
}

var _ planNode = &sequenceSelectNode{}

func (p *planner) SequenceSelectNode(desc catalog.TableDescriptor) (planNode, error) {
	if desc.GetSequenceOpts() == nil {
		return nil, errors.New("descriptor is not a sequence")
	}
	return &sequenceSelectNode{
		desc: desc,
	}, nil
}

func (ss *sequenceSelectNode) startExec(runParams) error {
	return nil
}

func (ss *sequenceSelectNode) Next(params runParams) (bool, error) {
	if ss.done {
		return false, nil
	}
	val, err := params.p.GetSequenceValue(params.ctx, params.ExecCfg().Codec, ss.desc)
	if err != nil {
		return false, err
	}
	ss.val = val
	ss.done = true
	return true, nil
}

func (ss *sequenceSelectNode) Values() tree.Datums {
	valDatum := tree.DInt(ss.val)
	cntDatum := tree.DInt(0)
	calledDatum := tree.DBoolTrue
	return []tree.Datum{
		&valDatum,
		&cntDatum,
		calledDatum,
	}
}

func (ss *sequenceSelectNode) Close(ctx context.Context) {}
