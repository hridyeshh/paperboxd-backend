package service

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/external"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// Read Now: store links resolved on a book's first detail view, and
// public-domain texts matched by the nightly backfill. See migration 000052.

const (
	// ReadLinksTTL is how long each source's answer in book_read_links is trusted.
	ReadLinksTTL = 30 * 24 * time.Hour
	// readLinksLookupTimeout caps the store lookups on a detail request.
	readLinksLookupTimeout = 3 * time.Second
)

type ReadLinksService struct {
	queries     *db.Queries
	googleBooks *external.GoogleBooksClient
	appleBooks  *external.AppleBooksClient
	gutenberg   *external.GutenbergClient
}

func NewReadLinksService(queries *db.Queries, googleBooks *external.GoogleBooksClient, appleBooks *external.AppleBooksClient, gutenberg *external.GutenbergClient) *ReadLinksService {
	return &ReadLinksService{queries: queries, googleBooks: googleBooks, appleBooks: appleBooks, gutenberg: gutenberg}
}

// Links is the read_links object for a book-detail response. It never fails:
// a cache or lookup error is logged and the links that need no lookup
// (Amazon, WorldCat, and whatever was cached) are still returned.
func (s *ReadLinksService) Links(ctx context.Context, book db.Book) *types.ReadLinks {
	row, err := s.queries.GetReadLinksByBookID(ctx, book.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("read links: cache read", "book_id", book.ID, "error", err)
	}
	if storeLookupDue(row.GooglePlayCheckedAt) || storeLookupDue(row.AppleBooksCheckedAt) {
		if fresh, resolveErr := s.Resolve(ctx, book, row); resolveErr != nil {
			slog.Warn("read links: resolve", "book_id", book.ID, "error", resolveErr)
		} else {
			row = fresh
		}
	}
	return BuildReadLinks(book, row)
}

// Resolve looks up the store links (Google Play, Apple Books) that are missing
// or older than ReadLinksTTL, and caches each store that answered. "Not
// carried" is an answer (link NULL, checked_at set); a lookup error or timeout
// is not, so that store alone is retried next view and nothing it had is lost.
// A book with no Google volume id or no ISBN is definitively not carried by
// that store. The error is for the cache write only; lookup errors are logged.
func (s *ReadLinksService) Resolve(ctx context.Context, book db.Book, row db.BookReadLink) (db.BookReadLink, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, readLinksLookupTimeout)
	defer cancel()

	var (
		wg                        sync.WaitGroup
		playLink, appleURL        string
		playChecked, appleChecked bool
	)
	if storeLookupDue(row.GooglePlayCheckedAt) {
		if !book.GoogleBooksID.Valid || book.GoogleBooksID.String == "" {
			playChecked = true
		} else {
			wg.Add(1)
			go func() {
				defer wg.Done()
				gb, err := s.googleBooks.GetByIDForStore(lookupCtx, book.GoogleBooksID.String)
				if err != nil {
					// ponytail: a store that keeps failing (e.g. no Google key, quota)
					// is retried every view, bounded by readLinksLookupTimeout; add a
					// failure backoff column if that shows in detail latency.
					slog.Warn("read links: google play lookup", "book_id", book.ID, "error", err)
					return
				}
				playLink, playChecked = gb.SaleInfo.BuyLink, true
			}()
		}
	}
	if storeLookupDue(row.AppleBooksCheckedAt) {
		if isbn := bookISBN(book); isbn == "" {
			appleChecked = true
		} else {
			wg.Add(1)
			go func() {
				defer wg.Done()
				link, err := s.appleBooks.LookupByISBN(lookupCtx, isbn)
				if err != nil {
					slog.Warn("read links: apple books lookup", "book_id", book.ID, "error", err)
					return
				}
				appleURL, appleChecked = link, true
			}()
		}
	}
	wg.Wait()
	if !playChecked && !appleChecked {
		return row, nil
	}

	return s.queries.UpsertStoreLinks(ctx, db.UpsertStoreLinksParams{
		BookID:            book.ID,
		GooglePlayBuyLink: optionalText(playLink),
		GoogleChecked:     playChecked,
		AppleBooksUrl:     optionalText(appleURL),
		AppleChecked:      appleChecked,
	})
}

func storeLookupDue(checkedAt pgtype.Timestamptz) bool {
	return !checkedAt.Valid || time.Since(checkedAt.Time) > ReadLinksTTL
}

// ResolvePublicDomain matches a book against Project Gutenberg and records
// the result, match or not. It calls Gutendex, so it runs from the backfill
// only — never on a request path. A lookup error writes nothing, so the book
// is retried next run.
func (s *ReadLinksService) ResolvePublicDomain(ctx context.Context, bookID uuid.UUID, title string, authors []string) (*external.GutenbergMatch, error) {
	match, err := s.gutenberg.Match(ctx, title, authors)
	if err != nil {
		return nil, err
	}

	params := db.UpsertPublicDomainParams{BookID: bookID}
	if match != nil {
		params.IsPublicDomain = true
		params.GutenbergID = pgtype.Int4{Int32: int32(match.ID), Valid: true}
		params.GutenbergHtmlUrl = optionalText(match.HTMLURL)
		params.GutenbergEpubUrl = optionalText(match.EPUBURL)
	}
	if _, err := s.queries.UpsertPublicDomain(ctx, params); err != nil {
		return nil, err
	}
	return match, nil
}

// PublicDomainRun is the outcome of one BackfillPublicDomain run.
type PublicDomainRun struct {
	Checked, Matched, NoMatch, Errored int
	// Aborted is set when Gutendex kept failing and the run stopped early.
	Aborted bool
}

const (
	// gutendexInterval spaces requests to Gutendex, a shared free instance.
	gutendexInterval = time.Second
	// gutendexMaxConsecutiveErrors stops a run while Gutendex is down instead
	// of timing out on every remaining book.
	gutendexMaxConsecutiveErrors = 5
)

// BackfillPublicDomain checks up to limit books against Project Gutenberg,
// most recently opened first, one Gutendex request per gutendexInterval.
// Every match is logged with both titles so it can be spot-checked.
func (s *ReadLinksService) BackfillPublicDomain(ctx context.Context, limit int32) (PublicDomainRun, error) {
	var run PublicDomainRun
	books, err := s.queries.ListBooksNeedingGutenbergCheck(ctx, limit)
	if err != nil {
		return run, err
	}

	tick := time.NewTicker(gutendexInterval)
	defer tick.Stop()
	consecutiveErrors := 0
	for i, b := range books {
		if i > 0 {
			select {
			case <-ctx.Done():
				return run, ctx.Err()
			case <-tick.C:
			}
		}

		run.Checked++
		match, err := s.ResolvePublicDomain(ctx, b.ID, b.Title, b.Authors)
		switch {
		case err != nil:
			run.Errored++
			consecutiveErrors++
			slog.Warn("gutenberg backfill: lookup", "book_id", b.ID, "title", b.Title, "error", err)
			if consecutiveErrors >= gutendexMaxConsecutiveErrors {
				run.Aborted = true
				return run, nil
			}
			continue
		case match == nil:
			run.NoMatch++
		default:
			run.Matched++
			slog.Info("gutenberg backfill: match", "book_id", b.ID, "title", b.Title, "authors", b.Authors,
				"gutenberg_title", match.Title, "gutenberg_id", match.ID)
		}
		consecutiveErrors = 0
	}
	return run, nil
}

// BuildReadLinks merges a cached row with the links built from the book
// itself. Amazon and WorldCat search by ISBN, or by title and author when a
// book has none, so they are always present.
func BuildReadLinks(book db.Book, row db.BookReadLink) *types.ReadLinks {
	links := &types.ReadLinks{
		GooglePlayBuyLink: textPtr(row.GooglePlayBuyLink),
		AppleBooksURL:     textPtr(row.AppleBooksUrl),
		IsPublicDomain:    row.IsPublicDomain,
		GutenbergHTMLURL:  textPtr(row.GutenbergHtmlUrl),
		GutenbergEPUBURL:  textPtr(row.GutenbergEpubUrl),
	}
	if row.GutenbergID.Valid {
		id := int(row.GutenbergID.Int32)
		links.GutenbergID = &id
	}

	if isbn := bookISBN(book); isbn != "" {
		links.AmazonSearchURL = "https://www.amazon.in/s?" + url.Values{"k": {isbn}, "i": {"digital-text"}}.Encode()
		links.WorldcatURL = "https://search.worldcat.org/isbn/" + isbn
	} else {
		q := strings.TrimSpace(book.Title + " " + strings.Join(book.Authors, " "))
		links.AmazonSearchURL = "https://www.amazon.in/s?" + url.Values{"k": {q}, "i": {"digital-text"}}.Encode()
		links.WorldcatURL = "https://search.worldcat.org/search?" + url.Values{"q": {q}}.Encode()
	}
	return links
}

func bookISBN(book db.Book) string {
	if !book.Isbn13.Valid {
		return ""
	}
	return strings.ReplaceAll(strings.TrimSpace(book.Isbn13.String), "-", "")
}

func optionalText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid || t.String == "" {
		return nil
	}
	return &t.String
}
