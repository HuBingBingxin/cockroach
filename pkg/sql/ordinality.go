// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package sql

import (
	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
)

// ordinalityNode represents a node that adds an "ordinality" column
// to its child node which numbers the rows it produces. Used to
// support WITH ORDINALITY.
//
// Note that the ordinalityNode produces results that number the *full
// set of original values*, as defined by the upstream data source
// specification. In particular, applying a filter before or after
// an intermediate ordinalityNode will produce different results.
//
// It is inserted in the logical plan between the renderNode and its
// source node, thus earlier than the WHERE filters.
//
// In other words, *ordinalityNode establishes a barrier to many
// common SQL optimizations*. Its use should be limited in clients to
// situations where the corresponding performance cost is affordable.
type ordinalityNode struct {
	source   planNode
	ordering orderingInfo
	columns  sqlbase.ResultColumns
	row      parser.Datums
	curCnt   int64
}

func (p *planner) wrapOrdinality(ds planDataSource) planDataSource {
	src := ds.plan
	srcColumns := planColumns(src)

	res := &ordinalityNode{
		source:   src,
		ordering: planOrdering(src),
		row:      make(parser.Datums, len(srcColumns)+1),
		curCnt:   1,
	}

	// Allocate an extra column for the ordinality values.
	res.columns = make(sqlbase.ResultColumns, len(srcColumns)+1)
	copy(res.columns, srcColumns)
	newColIdx := len(res.columns) - 1
	res.columns[newColIdx] = sqlbase.ResultColumn{
		Name: "ordinality",
		Typ:  parser.TypeInt,
	}

	// Extend the dataSourceInfo with information about the
	// new column.
	ds.info.sourceColumns = res.columns
	if srcIdx, ok := ds.info.sourceAliases.srcIdx(anonymousTable); !ok {
		ds.info.sourceAliases = append(ds.info.sourceAliases, sourceAlias{
			name:        anonymousTable,
			columnRange: []int{newColIdx},
		})
	} else {
		srcAlias := &ds.info.sourceAliases[srcIdx]
		srcAlias.columnRange = append(srcAlias.columnRange, newColIdx)
	}

	ds.plan = res

	return ds
}

func (o *ordinalityNode) Next(params runParams) (bool, error) {
	hasNext, err := o.source.Next(params)
	if !hasNext || err != nil {
		return hasNext, err
	}
	copy(o.row, o.source.Values())
	// o.row was allocated one spot larger than o.source.Values().
	// Store the ordinality value there.
	o.row[len(o.row)-1] = parser.NewDInt(parser.DInt(o.curCnt))
	o.curCnt++
	return true, nil
}

func (o *ordinalityNode) Values() parser.Datums        { return o.row }
func (o *ordinalityNode) Start(params runParams) error { return o.source.Start(params) }
func (o *ordinalityNode) Close(ctx context.Context)    { o.source.Close(ctx) }

// restrictOrdering transforms an ordering requirement on the output
// of an ordinalityNode into an ordering requirement on its input.
func (o *ordinalityNode) restrictOrdering(
	desiredOrdering sqlbase.ColumnOrdering,
) sqlbase.ColumnOrdering {
	// If there's a desired ordering on the ordinality column, drop it and every
	// column after that.
	if len(desiredOrdering) > 0 {
		for i, ordInfo := range desiredOrdering {
			if ordInfo.ColIdx == len(o.columns)-1 {
				return desiredOrdering[:i]
			}
		}
	}
	return desiredOrdering
}

// optimizeOrdering updates the ordinalityNode's ordering based on a
// potentially new ordering information from its source.
func (o *ordinalityNode) optimizeOrdering() {
	// We are going to "optimize" the ordering. We had an ordering
	// initially from the source, but expand() may have caused it to
	// change. So here retrieve the ordering of the source again.
	origOrdering := planOrdering(o.source)

	if len(origOrdering.ordering) > 0 {
		// TODO(knz/radu): we basically have two simultaneous orderings.
		// What we really want is something that orderingInfo cannot
		// currently express: that the rows are ordered by a set of
		// columns AND at the same time they are also ordered by a
		// different set of columns. However since ordinalityNode is
		// currently the only case where this happens we consider it's not
		// worth the hassle and just use the source ordering.
		o.ordering = origOrdering.copy()
	} else {
		// No ordering defined in the source, so create a new one.
		o.ordering.constantCols = origOrdering.constantCols.Copy()
		o.ordering.ordering = []orderingColumnGroup{{
			cols: util.MakeFastIntSet(uint32(len(o.columns) - 1)),
			dir:  encoding.Ascending,
		}}
		o.ordering.isKey = true
	}
}
