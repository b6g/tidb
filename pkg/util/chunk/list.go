// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package chunk

import (
	"unsafe"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/memory"
)

// List holds a slice of chunks, use to append rows with max chunk size properly handled.
type List struct {
	fieldTypes    []*types.FieldType
	initChunkSize int
	maxChunkSize  int
	length        int
	chunks        []*Chunk
	freelist      []*Chunk

	memTracker  *memory.Tracker // track memory usage.
	consumedIdx int             // chunk index in "chunks", has been consumed.
}

// RowPtrSize shows the size of RowPtr
const RowPtrSize = int(unsafe.Sizeof(RowPtr{}))

// RowPtr is used to get a row from a list.
// It is only valid for the list that returns it.
type RowPtr struct {
	ChkIdx uint32
	RowIdx uint32
}

// NewListWithMemTracker creates a new List with field types, init chunk size, max chunk size and memory tracker.
func NewListWithMemTracker(fieldTypes []*types.FieldType, initChunkSize, maxChunkSize int, tracker *memory.Tracker) *List {
	l := &List{
		fieldTypes:    fieldTypes,
		initChunkSize: initChunkSize,
		maxChunkSize:  maxChunkSize,
		memTracker:    tracker,
		consumedIdx:   -1,
	}
	return l
}

// NewList creates a new List with field types, init chunk size and max chunk size.
func NewList(fieldTypes []*types.FieldType, initChunkSize, maxChunkSize int) *List {
	return NewListWithMemTracker(fieldTypes, initChunkSize, maxChunkSize, memory.NewTracker(memory.LabelForChunkList, -1))
}

// GetMemTracker returns the memory tracker of this List.
func (l *List) GetMemTracker() *memory.Tracker {
	return l.memTracker
}

// Len returns the length of the List.
func (l *List) Len() int {
	return l.length
}

// NumChunks returns the number of chunks in the List.
func (l *List) NumChunks() int {
	return len(l.chunks)
}

// FieldTypes returns the fieldTypes of the list
func (l *List) FieldTypes() []*types.FieldType {
	return l.fieldTypes
}

// NumRowsOfChunk returns the number of rows of a chunk in the DataInDiskByRows.
func (l *List) NumRowsOfChunk(chkID int) int {
	return l.chunks[chkID].NumRows()
}

// GetChunk gets the Chunk by ChkIdx.
func (l *List) GetChunk(chkIdx int) *Chunk {
	return l.chunks[chkIdx]
}

// AppendRow appends a row to the List, the row is copied to the List.
func (l *List) AppendRow(row Row) RowPtr {
	chkIdx := len(l.chunks) - 1
	if chkIdx == -1 || l.chunks[chkIdx].NumRows() >= l.chunks[chkIdx].Capacity() || chkIdx == l.consumedIdx {
		newChk := l.allocChunk()
		l.chunks = append(l.chunks, newChk)
		if chkIdx != l.consumedIdx {
			l.memTracker.Consume(l.chunks[chkIdx].MemoryUsage())
			l.consumedIdx = chkIdx
		}
		chkIdx++
	}
	chk := l.chunks[chkIdx]
	rowIdx := chk.NumRows()
	chk.AppendRow(row)
	l.length++
	return RowPtr{ChkIdx: uint32(chkIdx), RowIdx: uint32(rowIdx)}
}

// Add adds a chunk to the List, the chunk may be modified later by the list.
// Caller must make sure the input chk is not empty and not used any more and has the same field types.
func (l *List) Add(chk *Chunk) {
	// FixMe: we should avoid add a Chunk that chk.NumRows() > list.maxChunkSize.
	if chk.NumRows() == 0 {
		// TODO: return error here.
		panic("chunk appended to List should have at least 1 row")
	}
	if chkIdx := len(l.chunks) - 1; l.consumedIdx != chkIdx {
		l.memTracker.Consume(l.chunks[chkIdx].MemoryUsage())
		l.consumedIdx = chkIdx
	}
	l.memTracker.Consume(chk.MemoryUsage())
	l.consumedIdx++
	l.chunks = append(l.chunks, chk)
	l.length += chk.NumRows()
}

func (l *List) allocChunk() (chk *Chunk) {
	if len(l.freelist) > 0 {
		lastIdx := len(l.freelist) - 1
		chk = l.freelist[lastIdx]
		l.freelist = l.freelist[:lastIdx]
		l.memTracker.Consume(-chk.MemoryUsage())
		chk.Reset()
		return
	}
	if len(l.chunks) > 0 {
		return Renew(l.chunks[len(l.chunks)-1], l.maxChunkSize)
	}
	return New(l.fieldTypes, l.initChunkSize, l.maxChunkSize)
}

// GetRow gets a Row from the list by RowPtr.
func (l *List) GetRow(ptr RowPtr) Row {
	chk := l.chunks[ptr.ChkIdx]
	return chk.GetRow(int(ptr.RowIdx))
}

// Reset resets the List.
func (l *List) Reset() {
	if lastIdx := len(l.chunks) - 1; lastIdx != l.consumedIdx {
		l.memTracker.Consume(l.chunks[lastIdx].MemoryUsage())
	}
	l.freelist = append(l.freelist, l.chunks...)
	l.chunks = l.chunks[:0]
	l.length = 0
	l.consumedIdx = -1
}

// Clear triggers GC for all the allocated chunks and reset the list
func (l *List) Clear() {
	l.memTracker.Consume(-l.memTracker.BytesConsumed())
	l.freelist = nil
	l.chunks = nil
	l.length = 0
	l.consumedIdx = -1
}

// ListWalkFunc is used to walk the list.
// If error is returned, it will stop walking.
type ListWalkFunc = func(row Row) error

// Walk iterate the list and call walkFunc for each row.
func (l *List) Walk(walkFunc ListWalkFunc) error {
	for i := range l.chunks {
		chk := l.chunks[i]
		for j := range chk.NumRows() {
			err := walkFunc(chk.GetRow(j))
			if err != nil {
				return errors.Trace(err)
			}
		}
	}
	return nil
}
