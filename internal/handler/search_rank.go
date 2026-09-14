package handler

import (
	"strings"
	"unicode"

	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// searchFillerWords are words a reader adds to describe a format rather than
// name a book, so "iron man comics" is judged on "iron" and "man".
// ponytail: fixed list; weight words by rarity if it starts misjudging queries.
var searchFillerWords = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "and": true, "by": true,
	"book": true, "books": true, "novel": true, "novels": true,
	"comic": true, "comics": true, "graphic": true, "manga": true,
	"series": true, "collection": true, "vol": true, "volume": true,
}

// queryWords returns the distinctive words of a search query, lowercased.
// A query made only of filler words keeps them all.
func queryWords(q string) []string {
	fields := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if !searchFillerWords[f] {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return fields
	}
	return out
}

// coversQuery reports whether every distinctive query word appears in the
// book's title, authors or ISBN. Fuzzy DB hits fail this ("Spider-Man Comics"
// for "iron man comics"); real matches and exact ISBNs pass.
func coversQuery(words []string, item types.BookResponse) bool {
	hay := strings.ToLower(item.VolumeInfo.Title + " " + strings.Join(item.VolumeInfo.Authors, " "))
	for _, id := range item.VolumeInfo.IndustryIdentifiers {
		hay += " " + id.Identifier
	}
	for _, w := range words {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return len(words) > 0
}

// splitByCoverage partitions results into those covering the query and the
// rest, keeping each group's order.
func splitByCoverage(words []string, items []types.BookResponse) (strong, weak []types.BookResponse) {
	for _, it := range items {
		if coversQuery(words, it) {
			strong = append(strong, it)
		} else {
			weak = append(weak, it)
		}
	}
	return strong, weak
}

// bookKeys identifies a result across sources, so an external hit already
// cached in the DB is not listed twice.
func bookKeys(it types.BookResponse) []string {
	var keys []string
	if it.GoogleBooksID != "" {
		keys = append(keys, "g:"+it.GoogleBooksID)
	}
	for _, id := range it.VolumeInfo.IndustryIdentifiers {
		if id.Type == "ISBN_13" && id.Identifier != "" {
			keys = append(keys, "i:"+id.Identifier)
		}
	}
	return keys
}

// mergeSearchResults orders one page: DB hits that name the query, external
// hits that name it, then fuzzy DB hits (typos, sound-alikes), then the rest.
func mergeSearchResults(words []string, dbItems, extItems []types.BookResponse, pageSize int) []types.BookResponse {
	seen := map[string]bool{}
	for _, it := range dbItems {
		for _, k := range bookKeys(it) {
			seen[k] = true
		}
	}
	fresh := make([]types.BookResponse, 0, len(extItems))
outer:
	for _, it := range extItems {
		keys := bookKeys(it)
		for _, k := range keys {
			if seen[k] {
				continue outer
			}
		}
		for _, k := range keys {
			seen[k] = true
		}
		fresh = append(fresh, it)
	}

	dbStrong, dbWeak := splitByCoverage(words, dbItems)
	extStrong, extWeak := splitByCoverage(words, fresh)
	out := make([]types.BookResponse, 0, len(dbItems)+len(fresh))
	out = append(out, dbStrong...)
	out = append(out, extStrong...)
	out = append(out, dbWeak...)
	out = append(out, extWeak...)
	if len(out) > pageSize {
		out = out[:pageSize]
	}
	return out
}
