package v1

import (
	"fmt"
	"sort"

	"github.com/efficientgo/core/errors"
	"github.com/prometheus/common/model"
)

type SeriesIterator interface {
	Iterator[*SeriesWithOffset]
	Reset()
}

type LazySeriesIter struct {
	b *Block

	// state
	initialized  bool
	err          error
	curPageIndex int
	curPage      *SeriesPageDecoder
	idx          int
}

// Decodes series pages one at a time and iterates through them
func NewLazySeriesIter(b *Block) *LazySeriesIter {
	return &LazySeriesIter{
		b: b,
	}
}

func NewLazySeriesIterWithIndex(b *Block, idx int) *LazySeriesIter {
	return &LazySeriesIter{
		b:   b,
		idx: idx,
	}
}

func (it *LazySeriesIter) ensureInit() {
	// TODO(owen-d): better control over when to decode
	if !it.initialized {
		if err := it.b.LoadHeaders(); err != nil {
			it.err = err
		}
		it.initialized = true
	}
}

// Seek returns an iterator over the pages where the first fingerprint is >= fp
func (it *LazySeriesIter) Seek(fp model.Fingerprint) error {
	fmt.Printf("LazySeriesIterator[%d].Seek(%d) - len pageheaders %d, curPageIdx %d\n", it.idx, fp, len(it.b.index.pageHeaders), it.curPageIndex)
	it.ensureInit()
	if it.err != nil {
		return it.err
	}

	// first potentially relevant page
	desiredPage := sort.Search(len(it.b.index.pageHeaders), func(i int) bool {
		header := it.b.index.pageHeaders[i]
		return header.ThroughFp >= fp
	})

	page := it.b.index.pageHeaders[desiredPage]

	switch {
	case desiredPage == len(it.b.index.pageHeaders), page.FromFp > fp:
		// no overlap exists, either because no page was found with a throughFP >= fp
		// or because the first page that was found has a fromFP > fp,
		// meaning successive pages would also have a fromFP > fp
		// since they're sorted in ascending fp order
		it.curPageIndex = len(it.b.index.pageHeaders)
		it.curPage = nil
		return nil

	case desiredPage == it.curPageIndex && it.curPage != nil:
		// on the right page, no action needed
	default:
		// need to load a new page
		r, err := it.b.reader.Index()
		if err != nil {
			return errors.Wrap(err, "getting index reader")
		}
		/*for i, hdr := range it.b.index.pageHeaders {
			fmt.Printf("LazySeriesIterator[%d].next() - page header[%d]: Num:%d, compressed: %d, uncompressed: %d, offset: %d\n", it.idx, i, hdr.NumSeries, hdr.Len, hdr.DecompressedLen, hdr.Offset)
		}*/
		fmt.Printf("LazySeriesIterator[%d].Seek(%d) - loading page %d\n", it.idx, fp, page.Offset)
		it.curPage, err = it.b.index.NewSeriesPageDecoder(
			r,
			page,
		)
		if err != nil {
			return err
		}
		it.curPageIndex = desiredPage
	}

	it.curPage.Seek(fp)
	return nil
}

func (it *LazySeriesIter) Next() bool {
	it.ensureInit()
	if it.err != nil {
		return false
	}

	return it.next()
}

func (it *LazySeriesIter) next() bool {
	for it.curPageIndex < len(it.b.index.pageHeaders) {
		// first access of next page
		if it.curPage == nil {
			curHeader := it.b.index.pageHeaders[it.curPageIndex]
			for i, hdr := range it.b.index.pageHeaders {
				fmt.Printf("LazySeriesIterator[%d].next() - page header[%d]: Num:%d, compressed: %d, uncompressed: %d, offset: %d\n", it.idx, i, hdr.NumSeries, hdr.Len, hdr.DecompressedLen, hdr.Offset)
			}
			r, err := it.b.reader.Index()
			if err != nil {
				it.err = errors.Wrap(err, "getting index reader")
				return false
			}
			fmt.Printf("LazySeriesIterator[%d].next() - loading page %d\n", it.idx, curHeader.Offset)

			it.curPage, err = it.b.index.NewSeriesPageDecoder(
				r,
				curHeader,
			)
			if err != nil {
				it.err = err
				return false
			}
			continue
		}

		if !it.curPage.Next() {
			// there was an error
			if it.curPage.Err() != nil {
				return false
			}

			// we've exhausted the current page, progress to next
			it.curPageIndex++
			it.curPage = nil
			continue
		}

		return true
	}

	// finished last page
	return false
}

func (it *LazySeriesIter) At() *SeriesWithOffset {
	return it.curPage.At()
}

func (it *LazySeriesIter) Err() error {
	if it.err != nil {
		return it.err
	}
	if it.curPage != nil {
		return it.curPage.Err()
	}
	return nil
}
