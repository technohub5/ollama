package cache

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/ollama/ollama/ml"
)

type Options struct {
	Sequences []int
}

type Cache interface {
	// used by model implementations
	Sub(i int) Cache
	Put(ctx ml.Context, key, value ml.Tensor, opts Options) (ml.Tensor, ml.Tensor, ml.Tensor)

	// cache management
	Close()

	StartForward(ctx ml.Context, seqs []int) error

	Copy(srcSeq, dstSeq int, beginIndex, endIndex int)
	Shift(seq int, beginIndex, endIndex, offset int)
	Remove(seq int, beginIndex, endIndex int) error
}

type Simple struct {
	DType    ml.DType
	Capacity int

	// current forward pass
	curLayer     int
	curPos       int
	curBatchSize int
	curMask      ml.Tensor

	// metadata
	size       int
	cells      []cacheCell
	maxCell    int
	seqNextPos map[int]int

	// cache data storage
	cacheCtx     ml.Context
	keys, values []ml.Tensor
}

type seqCell struct {
	seq int
	pos int
}

type cacheCell struct {
	sequences []seqCell
}

// TODO(jessegross): Can use IndexFunc?
func (cell cacheCell) findSeq(seq int) *seqCell {
	for i := range cell.sequences {
		if cell.sequences[i].seq == seq {
			return &cell.sequences[i]
		}
	}
	return nil
}

func NewSimpleCache(backend ml.Backend, capacity int, dtype ml.DType) Cache {
	return &Simple{
		Capacity:   capacity,
		DType:      dtype,
		cells:      make([]cacheCell, capacity),
		seqNextPos: make(map[int]int),
		// TODO(jessegross): This context is not sized appropriately
		cacheCtx: backend.NewContext(),
	}
}

func (c *Simple) Close() {
	c.cacheCtx.Close()
}

var ErrKvCacheFull = errors.New("could not find a kv cache slot")

func (c *Simple) StartForward(ctx ml.Context, seqs []int) error {
	c.curBatchSize = len(seqs)

	var err error
	c.curPos, err = c.findStartPos()
	if errors.Is(err, ErrKvCacheFull) && c.size+c.curBatchSize <= c.Capacity {
		c.defrag()
		c.curPos, err = c.findStartPos()
	}
	if err != nil {
		return err
	}

	// TODO(jessegross): There should be a better way to do this
	origSeq := make(map[int]int)
	for k, v := range c.seqNextPos {
		origSeq[k] = v
	}

	for i, seq := range seqs {
		c.cells[c.curPos+i] = cacheCell{sequences: []seqCell{{seq: seq, pos: c.seqNextPos[seq]}}}
		if c.curPos+i > c.maxCell {
			c.maxCell = c.curPos + i
		}
		c.seqNextPos[seq]++
		c.size++
	}

	c.curMask, err = c.buildMask(ctx, origSeq, seqs)

	return err
}

func (c *Simple) findStartPos() (int, error) {
	var start, count int
	for i := range c.cells {
		if len(c.cells[i].sequences) == 0 {
			count++
			if count >= c.curBatchSize {
				return start, nil
			}
		} else {
			start = i + 1
			count = 0
		}
	}

	return 0, fmt.Errorf("%w (length: %v)", ErrKvCacheFull, c.Capacity)
}

func (c *Simple) buildMask(ctx ml.Context, origSeq map[int]int, seqs []int) (ml.Tensor, error) {
	// TODO(jessegross): This makes a number of simplifications, including assuming
	// causal attention, no padding, etc.
	mask := make([]float32, c.curBatchSize*c.maxCell)

	// TODO(jessegross): Only compute over the cells that have our sequences
	for i := range c.curBatchSize {
		for j := range c.maxCell {
			cellSeq := c.cells[j].findSeq(seqs[i])
			if cellSeq == nil || cellSeq.pos > origSeq[seqs[i]]+i {
				mask[i*c.maxCell+j] = float32(math.Inf(-1))
			}
		}
	}

	return ctx.FromFloatSlice(mask, c.maxCell, c.curBatchSize)
}

func (c *Simple) defrag() {
	panic("Defrag not yet implemented")
}

func (c *Simple) Sub(i int) Cache {
	if i >= len(c.keys) {
		c.keys = append(c.keys, make([]ml.Tensor, i-len(c.keys)+1)...)
		c.values = append(c.values, make([]ml.Tensor, i-len(c.values)+1)...)
	}

	c.curLayer = i

	return c
}

func (c *Simple) Put(ctx ml.Context, key, value ml.Tensor, opts Options) (ml.Tensor, ml.Tensor, ml.Tensor) {
	if c.curBatchSize != int(key.Dim(2)) {
		panic(fmt.Errorf("inconsistent batch sizes (layer: %v, batch size: %v layer batch size: %v)", c.curLayer, c.curBatchSize, int(key.Dim(2))))
	}

	if c.keys[c.curLayer] == nil || c.values[c.curLayer] == nil {
		c.keys[c.curLayer] = c.cacheCtx.Zeros(c.DType, int(key.Dim(0)*key.Dim(1))*c.Capacity)
		c.values[c.curLayer] = c.cacheCtx.Zeros(c.DType, int(value.Dim(0)*value.Dim(1))*c.Capacity)
	}

	ctx.Forward(key.Copy(ctx, c.keys[c.curLayer].View(ctx, int(key.Stride(2))*c.curPos, int(key.Dim(0)*key.Dim(1)*key.Dim(2)))))
	ctx.Forward(value.Copy(ctx, c.values[c.curLayer].View(ctx, int(value.Stride(2))*c.curPos, int(value.Dim(0)*value.Dim(1)*value.Dim(2)))))

	key = c.keys[c.curLayer].View(ctx, 0,
		int(key.Dim(0)), int(key.Stride(1)),
		int(key.Dim(1)), int(key.Stride(2)),
		c.maxCell,
	)

	value = c.values[c.curLayer].View(ctx, 0,
		int(value.Dim(0)), int(value.Stride(1)),
		int(value.Dim(1)), int(value.Stride(2)),
		c.maxCell,
	)

	return key, value, c.curMask
}

func (c *Simple) Copy(srcSeq, dstSeq int, beginIndex, endIndex int) {
	endIndex = min(endIndex, c.seqNextPos[srcSeq])
	offset := endIndex - beginIndex

	for i := range c.cells {
		srcCellSeq := c.cells[i].findSeq(srcSeq)
		if srcCellSeq != nil && srcCellSeq.pos >= beginIndex && srcCellSeq.pos < endIndex {
			c.cells[i].sequences = append(c.cells[i].sequences, seqCell{seq: dstSeq, pos: srcCellSeq.pos})
		}

		dstCellSeq := c.cells[i].findSeq(dstSeq)
		if dstCellSeq != nil && dstCellSeq.pos >= endIndex {
			dstCellSeq.pos += offset
		}
	}

	c.seqNextPos[dstSeq] += offset
}

func (c *Simple) Shift(seq int, beginIndex, endIndex, offset int) {
	panic("Shift not yet implemented")
}

var ErrNotSupported = errors.New("model does not support operation")

func (c *Simple) Remove(seq int, beginIndex, endIndex int) error {
	endIndex = min(endIndex, c.seqNextPos[seq])
	offset := endIndex - beginIndex

	// TODO(jessegross): Some models don't support partial erasure
	for i := range c.cells {
		cellSeq := c.cells[i].findSeq(seq)
		if cellSeq != nil {
			if cellSeq.pos >= beginIndex && cellSeq.pos < endIndex {
				c.cells[i].sequences = slices.DeleteFunc(c.cells[i].sequences, func(s seqCell) bool { return s.seq == seq })
			} else if cellSeq.pos >= endIndex {
				cellSeq.pos -= offset
			}
		}
	}

	c.seqNextPos[seq] -= offset
	c.size -= offset

	return nil
}
