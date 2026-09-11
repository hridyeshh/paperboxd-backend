package handler

import "testing"

// The rating shown on a book page is rounded to one decimal; a book with no
// Paperboxd ratings must report nil so clients fall back to the publisher's
// number rather than printing 0.0 as if readers hated it.
func TestReaderRatingRounding(t *testing.T) {
	cases := []struct {
		avg   float64
		count int32
		want  *float64
	}{
		{avg: 0, count: 0, want: nil},
		{avg: 4.25, count: 4, want: f(4.3)},
		{avg: 3.333333, count: 3, want: f(3.3)},
		{avg: 5, count: 1, want: f(5)},
	}
	for _, c := range cases {
		var got *float64
		if c.count > 0 {
			r := float64(int(c.avg*10+0.5)) / 10
			got = &r
		}
		switch {
		case c.want == nil && got != nil:
			t.Fatalf("avg %v count %d: want nil, got %v", c.avg, c.count, *got)
		case c.want != nil && got == nil:
			t.Fatalf("avg %v count %d: want %v, got nil", c.avg, c.count, *c.want)
		case c.want != nil && *got != *c.want:
			t.Fatalf("avg %v count %d: want %v, got %v", c.avg, c.count, *c.want, *got)
		}
	}
}

func f(v float64) *float64 { return &v }
