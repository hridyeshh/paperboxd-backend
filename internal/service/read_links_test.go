package service

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/hridyesh/paperboxd-backend/internal/db"
)

func TestBuildReadLinksByISBN(t *testing.T) {
	book := db.Book{Title: "Dracula", Authors: []string{"Bram Stoker"}, Isbn13: pgtype.Text{String: "978-0141439518", Valid: true}}
	links := BuildReadLinks(book, db.BookReadLink{})

	if links.AmazonSearchURL != "https://www.amazon.in/s?i=digital-text&k=9780141439518" {
		t.Errorf("amazon = %q", links.AmazonSearchURL)
	}
	if links.WorldcatURL != "https://search.worldcat.org/isbn/9780141439518" {
		t.Errorf("worldcat = %q", links.WorldcatURL)
	}
	// An empty cache row must read as "no store link", not as an empty URL.
	if links.GooglePlayBuyLink != nil || links.AppleBooksURL != nil || links.GutenbergHTMLURL != nil || links.GutenbergID != nil {
		t.Errorf("empty row produced links: %+v", links)
	}
}

// Books cached without an ISBN still get Amazon and WorldCat, by title and author.
func TestBuildReadLinksWithoutISBN(t *testing.T) {
	links := BuildReadLinks(db.Book{Title: "Pride & Prejudice", Authors: []string{"Jane Austen"}}, db.BookReadLink{})

	if !strings.HasPrefix(links.AmazonSearchURL, "https://www.amazon.in/s?") || !strings.Contains(links.AmazonSearchURL, "k=Pride+%26+Prejudice+Jane+Austen") {
		t.Errorf("amazon = %q", links.AmazonSearchURL)
	}
	if links.WorldcatURL != "https://search.worldcat.org/search?q=Pride+%26+Prejudice+Jane+Austen" {
		t.Errorf("worldcat = %q", links.WorldcatURL)
	}
}

func TestBuildReadLinksCarriesCachedRow(t *testing.T) {
	row := db.BookReadLink{
		GooglePlayBuyLink: pgtype.Text{String: "https://play.google.com/store/books/details?id=x", Valid: true},
		IsPublicDomain:    true,
		GutenbergID:       pgtype.Int4{Int32: 345, Valid: true},
		GutenbergHtmlUrl:  pgtype.Text{String: "https://www.gutenberg.org/ebooks/345.html.images", Valid: true},
	}
	links := BuildReadLinks(db.Book{Title: "Dracula"}, row)

	if links.GooglePlayBuyLink == nil || !links.IsPublicDomain || links.GutenbergID == nil || *links.GutenbergID != 345 || links.GutenbergHTMLURL == nil {
		t.Errorf("cached row lost: %+v", links)
	}
}
