package service

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Fusion story assembly.
//
// Everything here is pure: the DB side (fusion.go) loads both readers into a
// fusionInput and this turns it into the ten-page story from one reader's
// side. The same "never fabricate reasons" rule as the recommendation reasons
// applies: every sentence is gated on the evidence that makes it true, and a
// section with no honest content is left empty for the client to skip.

// FusionPerson is one half of a Fusion.
type FusionPerson struct {
	UserID    string `json:"user_id"`
	Username  string `json:"username"`
	Name      string `json:"name"`
	First     string `json:"first"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

// FusionBook is a book as the story shows it. You/Them are star ratings where
// the story has a reason to show them.
type FusionBook struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Slug     string   `json:"slug,omitempty"`
	Authors  []string `json:"authors"`
	CoverURL string   `json:"cover_url,omitempty"`
	You      *int     `json:"you,omitempty"`
	Them     *int     `json:"them,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Match    string   `json:"match,omitempty"`
}

// FusionAgree is one thing both readers want. Kind "axis" is a taste axis
// (You/Them are 0..100 toward the phrase); kind "genre" falls back to shelf
// share when trait profiles are not built yet.
type FusionAgree struct {
	Kind   string `json:"kind"`
	Key    string `json:"key"`
	Phrase string `json:"phrase"`
	You    int    `json:"you"`
	Them   int    `json:"them"`
}

// FusionSplit is one place the two readers pull apart.
type FusionSplit struct {
	Label string `json:"label"`
	You   string `json:"you"`
	Them  string `json:"them"`
}

// FusionReason is one line under a pick. Who is "both", "you" or "them".
type FusionReason struct {
	Who  string `json:"who"`
	Text string `json:"text"`
}

// FusionPick is the hero pick or the wildcard.
type FusionPick struct {
	Book    FusionBook     `json:"book"`
	Label   string         `json:"label"`
	Reasons []FusionReason `json:"reasons"`
}

// FusionView is the whole story from the viewer's side.
type FusionView struct {
	ID        string       `json:"id"`
	Me        FusionPerson `json:"me"`
	Them      FusionPerson `json:"them"`
	Score     int          `json:"score"`
	ScoreLine string       `json:"score_line"`

	SharedCount   int      `json:"shared_count"`
	Agreement     int      `json:"agreement"` // % of books both rated within one star
	LovedCount    int      `json:"loved_count"`
	SharedAuthors []string `json:"shared_authors"`
	SharedGenres  []string `json:"shared_genres"`

	SharedLoved  []FusionBook  `json:"shared_loved"`
	Agree        []FusionAgree `json:"agree"`
	Splits       []FusionSplit `json:"splits"`
	Disagreement *FusionBook   `json:"disagreement"`
	ForYou       []FusionBook  `json:"for_you"`
	ForThem      []FusionBook  `json:"for_them"`
	Pick         *FusionPick   `json:"pick"`
	Also         []FusionBook  `json:"also"`
	Wildcard     *FusionPick   `json:"wildcard"`
	Both         []FusionBook  `json:"both"`

	WaitingCount int    `json:"waiting_count"`
	Verdict      string `json:"verdict"`
	VerdictSub   string `json:"verdict_sub"`

	// LowData is true when there is too little on either shelf for the
	// number to mean anything; the client shows the low-data page instead.
	LowData    bool      `json:"low_data"`
	ComputedAt time.Time `json:"computed_at"`
}

// fusionSnapshot is what fusions.snapshot stores: the story from each side.
type fusionSnapshot struct {
	A FusionView `json:"a"`
	B FusionView `json:"b"`
}

type fusionBookMeta struct {
	Title      string
	Slug       string
	Authors    []string
	CoverURL   string
	Categories []string
}

type fusionSide struct {
	Person      FusionPerson
	Shelf       readerShelf        // read/reading/liked or rated, with Traits
	AllIDs      map[string]bool    // every shelved book, to-read included
	Genres      map[string]float64 // signal profile genre weights
	MedianPages int                // 0 = unknown
	Recs        []BookCandidate    // home recommendations, best first
}

type fusionInput struct {
	A, B          fusionSide
	Books         map[string]fusionBookMeta
	BookTraits    map[string]map[string]float64
	SharedAuthors []string
	Now           time.Time
}

func (in fusionInput) flipped() fusionInput {
	in.A, in.B = in.B, in.A
	return in
}

const (
	fusionShowLoved   = 6
	fusionShowShelf   = 3
	fusionShowAgree   = 4
	fusionShowSplits  = 3
	fusionShowBoth    = 5
	fusionGenreShare  = 0.10 // a genre "counts" for a reader above this share
	fusionAxisAgree   = 0.15 // prefs this close agree
	fusionAxisSplit   = 0.35 // prefs this far apart, on opposite sides, split
	fusionLengthRatio = 1.4
)

// buildFusionView assembles the story from A's side.
func buildFusionView(in fusionInput) FusionView {
	a, b := in.A, in.B
	v := FusionView{
		Me:            a.Person,
		Them:          b.Person,
		SharedAuthors: nonNil(in.SharedAuthors),
		ComputedAt:    in.Now,
	}
	v.Score = int(math.Round(ComputeOverlap(a.Shelf, b.Shelf).Overlap * 100))

	// Shared shelf: counts, loved, the biggest disagreement.
	var rated, within int
	var loved []string
	var worst string
	worstGap := 1
	for id, ra := range a.Shelf.Books {
		rb, ok := b.Shelf.Books[id]
		if !ok {
			continue
		}
		v.SharedCount++
		if ra == nil || rb == nil {
			continue
		}
		rated++
		gap := absInt(*ra - *rb)
		if gap <= 1 {
			within++
		}
		if *ra >= 4 && *rb >= 4 {
			loved = append(loved, id)
		}
		if gap > worstGap || (gap == worstGap && worst != "" && in.title(id) < in.title(worst)) {
			worst, worstGap = id, gap
		}
	}
	if rated > 0 {
		v.Agreement = int(math.Round(float64(within) / float64(rated) * 100))
	}
	v.LovedCount = len(loved)
	sort.Slice(loved, func(i, j int) bool {
		si := *a.Shelf.Books[loved[i]] + *b.Shelf.Books[loved[i]]
		sj := *a.Shelf.Books[loved[j]] + *b.Shelf.Books[loved[j]]
		if si != sj {
			return si > sj
		}
		return in.title(loved[i]) < in.title(loved[j])
	})
	for _, id := range capStrings(loved, fusionShowLoved) {
		v.SharedLoved = append(v.SharedLoved, in.book(id, a.Shelf.Books[id], b.Shelf.Books[id]))
	}
	if worst != "" && worstGap >= 2 {
		d := in.book(worst, a.Shelf.Books[worst], b.Shelf.Books[worst])
		v.Disagreement = &d
	}

	sa, sb := genreShares(a.Genres), genreShares(b.Genres)
	v.SharedGenres = sharedGenres(sa, sb)
	v.Agree = in.agree(sa, sb)
	v.Splits = in.splits(sa, sb)

	v.ForYou = in.couldRead(b, a, func(n int, phrase string) (string, string) {
		if phrase != "" {
			return fmt.Sprintf("%s loved this. It sits right on your taste for %s.", b.Person.First, phrase), "Strong match"
		}
		if n == 5 {
			return fmt.Sprintf("%s gave it five stars.", b.Person.First), "Great fit"
		}
		return fmt.Sprintf("%s rated it %d.", b.Person.First, n), "Interesting match"
	})
	v.ForThem = in.couldRead(a, b, func(n int, phrase string) (string, string) {
		if phrase != "" {
			return fmt.Sprintf("You loved this. It sits right on %s's taste for %s.", b.Person.First, phrase), "Strong match"
		}
		if n == 5 {
			return "You gave it five stars.", "Great fit"
		}
		return fmt.Sprintf("You rated it %d.", n), "Interesting match"
	})

	in.picks(&v, sa, sb)

	seen := map[string]bool{}
	for _, list := range [][]FusionBook{v.Both, v.ForYou, v.ForThem} {
		for _, bk := range list {
			seen[bk.ID] = true
		}
	}
	v.WaitingCount = len(seen)

	v.LowData = len(a.Shelf.Books) < 3 || len(b.Shelf.Books) < 3 ||
		(v.SharedCount < overlapMinShared && !(a.Shelf.Traits.HasSignal() && b.Shelf.Traits.HasSignal()))

	switch {
	case v.Score >= 75:
		v.ScoreLine = "You two have surprisingly good taste together."
		v.Verdict = "Your tastes make more sense together than apart."
	case v.Score >= 50:
		v.ScoreLine = "A lot of common ground, and a few surprises."
		v.Verdict = "Enough in common to trust each other's picks."
	default:
		v.ScoreLine = "You two read very differently."
		v.Verdict = "Different shelves. That is the whole point."
	}
	switch v.WaitingCount {
	case 0:
		v.VerdictSub = "Rate a few more books and PaperBoxd will find some for both of you."
	case 1:
		v.VerdictSub = "PaperBoxd found one book waiting for both of you."
	default:
		v.VerdictSub = fmt.Sprintf("PaperBoxd found %d books waiting for both of you.", v.WaitingCount)
	}

	v.SharedLoved = nonNilBooks(v.SharedLoved)
	v.ForYou = nonNilBooks(v.ForYou)
	v.ForThem = nonNilBooks(v.ForThem)
	v.Also = nonNilBooks(v.Also)
	v.Both = nonNilBooks(v.Both)
	if v.Agree == nil {
		v.Agree = []FusionAgree{}
	}
	if v.Splits == nil {
		v.Splits = []FusionSplit{}
	}
	return v
}

// couldRead is what owner loved that reader has not shelved, best first.
func (in fusionInput) couldRead(owner, reader fusionSide, reason func(rating int, phrase string) (string, string)) []FusionBook {
	type row struct {
		id     string
		rating int
		phrase string
	}
	var rows []row
	for id, r := range owner.Shelf.Books {
		if r == nil || *r < 4 || reader.AllIDs[id] || in.Books[id].Title == "" {
			continue
		}
		rows = append(rows, row{id, *r, in.phrase(id, reader.Shelf.Traits)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].rating != rows[j].rating {
			return rows[i].rating > rows[j].rating
		}
		if (rows[i].phrase != "") != (rows[j].phrase != "") {
			return rows[i].phrase != ""
		}
		return in.title(rows[i].id) < in.title(rows[j].id)
	})
	var out []FusionBook
	for _, r := range rows {
		if len(out) == fusionShowShelf {
			break
		}
		var bk FusionBook
		if owner.Person.UserID == in.A.Person.UserID {
			bk = in.book(r.id, owner.Shelf.Books[r.id], nil)
		} else {
			bk = in.book(r.id, nil, owner.Shelf.Books[r.id])
		}
		bk.Reason, bk.Match = reason(r.rating, r.phrase)
		out = append(out, bk)
	}
	return out
}

// agree lists axes both readers sit on together, strongest first, falling back
// to shared genres while trait profiles are empty.
func (in fusionInput) agree(sa, sb map[string]float64) []FusionAgree {
	ta, tb := in.A.Shelf.Traits, in.B.Shelf.Traits
	type scored struct {
		FusionAgree
		strength float64
	}
	var rows []scored
	if ta.HasSignal() && tb.HasSignal() {
		for _, axis := range TraitAxes {
			ca, cb := ta.Confidence[axis], tb.Confidence[axis]
			if ca <= 0 || cb <= 0 {
				continue
			}
			va, vb := ta.Prefs[axis], tb.Prefs[axis]
			avg := (va + vb) / 2
			strength := math.Abs(avg-0.5) * 2 * math.Min(ca, cb)
			if math.Abs(va-vb) > fusionAxisAgree || strength < traitReasonMinStrength {
				continue
			}
			high := avg > 0.5
			rows = append(rows, scored{FusionAgree{
				Kind:   "axis",
				Key:    axis,
				Phrase: capitalise(axisPhrase(axis, high)),
				You:    toward(va, high),
				Them:   toward(vb, high),
			}, strength})
		}
	}
	if len(rows) == 0 {
		for _, g := range sharedGenres(sa, sb) {
			rows = append(rows, scored{FusionAgree{
				Kind: "genre", Key: g, Phrase: g,
				You: pct(sa[g]), Them: pct(sb[g]),
			}, math.Min(sa[g], sb[g])})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].strength > rows[j].strength })
	var out []FusionAgree
	for i, r := range rows {
		if i == fusionShowAgree {
			break
		}
		out = append(out, r.FusionAgree)
	}
	return out
}

// splits lists where the readers pull apart: opposite sides of a taste axis,
// very different book lengths, a genre one reads and the other does not.
func (in fusionInput) splits(sa, sb map[string]float64) []FusionSplit {
	var out []FusionSplit
	ta, tb := in.A.Shelf.Traits, in.B.Shelf.Traits
	if ta.HasSignal() && tb.HasSignal() {
		type gap struct {
			axis string
			d    float64
		}
		var gaps []gap
		for _, axis := range TraitAxes {
			if ta.Confidence[axis] <= 0 || tb.Confidence[axis] <= 0 {
				continue
			}
			va, vb := ta.Prefs[axis], tb.Prefs[axis]
			if math.Abs(va-vb) >= fusionAxisSplit && (va-0.5)*(vb-0.5) < 0 {
				gaps = append(gaps, gap{axis, math.Abs(va - vb)})
			}
		}
		sort.SliceStable(gaps, func(i, j int) bool { return gaps[i].d > gaps[j].d })
		for _, g := range gaps {
			out = append(out, FusionSplit{
				Label: axisLabel[g.axis],
				You:   capitalise(axisPhrase(g.axis, ta.Prefs[g.axis] > 0.5)),
				Them:  capitalise(axisPhrase(g.axis, tb.Prefs[g.axis] > 0.5)),
			})
		}
	}

	pa, pb := in.A.MedianPages, in.B.MedianPages
	if pa > 0 && pb > 0 && float64(max(pa, pb))/float64(min(pa, pb)) >= fusionLengthRatio {
		out = append(out, FusionSplit{
			Label: "Book length",
			You:   fmt.Sprintf("Around %d pages", roundTo(pa, 50)),
			Them:  fmt.Sprintf("Around %d pages", roundTo(pb, 50)),
		})
	}

	type genreGap struct {
		g string
		d float64
	}
	var gg []genreGap
	for g := range unionKeys(sa, sb) {
		hi, lo := math.Max(sa[g], sb[g]), math.Min(sa[g], sb[g])
		if hi >= 0.15 && (lo == 0 || hi/lo >= 3) {
			gg = append(gg, genreGap{g, hi - lo})
		}
	}
	sort.Slice(gg, func(i, j int) bool {
		if gg[i].d != gg[j].d {
			return gg[i].d > gg[j].d
		}
		return gg[i].g < gg[j].g
	})
	for _, g := range gg {
		out = append(out, FusionSplit{Label: g.g, You: shareText(sa[g.g]), Them: shareText(sb[g.g])})
	}

	if len(out) > fusionShowSplits {
		out = out[:fusionShowSplits]
	}
	return out
}

// picks fills Pick, Also, Wildcard and Both from the two recommendation lists.
// A book both lists carry ranks first; nothing either reader has shelved is
// eligible, so "neither of you has read it" is always true.
func (in fusionInput) picks(v *FusionView, sa, sb map[string]float64) {
	type cand struct {
		bc     BookCandidate
		score  float64
		inBoth bool
		wild   bool
	}
	byID := map[string]*cand{}
	var order []string
	add := func(recs []BookCandidate) {
		for i, bc := range recs {
			if in.A.AllIDs[bc.ID] || in.B.AllIDs[bc.ID] || bc.ID == "" {
				continue
			}
			c, ok := byID[bc.ID]
			if !ok {
				c = &cand{bc: bc}
				byID[bc.ID] = c
				order = append(order, bc.ID)
			} else {
				c.inBoth = true
			}
			c.score += 1 / float64(1+i)
			c.wild = c.wild || bc.Confidence == "Wild card" || bc.ReasonType == "explore"
		}
	}
	add(in.A.Recs)
	add(in.B.Recs)
	if len(order) == 0 {
		return
	}
	ranked := make([]*cand, 0, len(order))
	for _, id := range order {
		ranked = append(ranked, byID[id])
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].inBoth != ranked[j].inBoth {
			return ranked[i].inBoth
		}
		return ranked[i].score > ranked[j].score
	})

	used := map[string]bool{}
	fb := func(c *cand, label string) FusionBook {
		bk := FusionBook{ID: c.bc.ID, Title: c.bc.Title, Authors: nonNil(c.bc.Authors), CoverURL: c.bc.CoverURL, Match: label}
		bk.Slug = in.Books[c.bc.ID].Slug
		used[c.bc.ID] = true
		return bk
	}
	label := func(c *cand) string {
		if c.inBoth {
			return "Strong Fusion pick"
		}
		return "Great fit"
	}

	// Wildcard first, so the hero does not take the only stretch pick.
	common := map[string]bool{}
	for g, s := range sa {
		if s >= fusionGenreShare {
			common[strings.ToLower(g)] = true
		}
	}
	for g, s := range sb {
		if s >= fusionGenreShare {
			common[strings.ToLower(g)] = true
		}
	}
	for _, c := range ranked {
		if !c.wild || len(c.bc.Categories) == 0 {
			continue
		}
		usual := false
		for _, cat := range c.bc.Categories {
			if common[strings.ToLower(cat)] {
				usual = true
				break
			}
		}
		if usual {
			continue
		}
		reasons := []FusionReason{{Who: "both", Text: "Neither of you usually reads this."}}
		if pa, pb := in.phrase(c.bc.ID, in.A.Shelf.Traits), in.phrase(c.bc.ID, in.B.Shelf.Traits); pa != "" && pa == pb {
			reasons = append(reasons, FusionReason{Who: "both", Text: "But you both love " + pa + ", and it is one."})
		}
		v.Wildcard = &FusionPick{Book: fb(c, "Wildcard"), Label: "Wildcard", Reasons: reasons}
		break
	}

	for _, c := range ranked {
		if used[c.bc.ID] {
			continue
		}
		if v.Pick == nil {
			reasons := []FusionReason{{Who: "both", Text: "Neither of you has read it."}}
			if c.inBoth {
				reasons = append(reasons, FusionReason{Who: "both", Text: "PaperBoxd would recommend it to either of you on its own."})
			}
			pa, pb := in.phrase(c.bc.ID, in.A.Shelf.Traits), in.phrase(c.bc.ID, in.B.Shelf.Traits)
			switch {
			case pa != "" && pa == pb:
				reasons = append(reasons, FusionReason{Who: "both", Text: "You both love " + pa + "."})
			default:
				if pb != "" {
					reasons = append(reasons, FusionReason{Who: "them", Text: fmt.Sprintf("%s leans toward %s.", in.B.Person.First, pb)})
				}
				if pa != "" {
					reasons = append(reasons, FusionReason{Who: "you", Text: "It sits right on your taste for " + pa + "."})
				}
			}
			l := label(c)
			v.Pick = &FusionPick{Book: fb(c, l), Label: l, Reasons: reasons}
			continue
		}
		if len(v.Also) < 2 {
			v.Also = append(v.Also, fb(c, label(c)))
		}
		if len(v.Also) == 2 {
			break
		}
	}

	if v.Pick != nil {
		v.Both = append(v.Both, v.Pick.Book)
	}
	v.Both = append(v.Both, v.Also...)
	if v.Wildcard != nil {
		v.Both = append(v.Both, v.Wildcard.Book)
	}
	for _, c := range ranked {
		if len(v.Both) >= fusionShowBoth {
			break
		}
		if !used[c.bc.ID] {
			v.Both = append(v.Both, fb(c, label(c)))
		}
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (in fusionInput) title(id string) string { return in.Books[id].Title }

func (in fusionInput) book(id string, you, them *int) FusionBook {
	m := in.Books[id]
	return FusionBook{ID: id, Title: m.Title, Slug: m.Slug, Authors: nonNil(m.Authors), CoverURL: m.CoverURL, You: you, Them: them}
}

// phrase is the reader's taste phrase this book actually sits on, or "".
func (in fusionInput) phrase(id string, p *TraitProfile) string {
	t := in.BookTraits[id]
	if t == nil {
		return ""
	}
	if m := matchedTraitPhrases(Candidate{BookID: id, Traits: t}, p, 1); len(m) == 1 {
		return m[0]
	}
	return ""
}

func axisPhrase(axis string, high bool) string {
	p := traitPhrases[axis]
	if high {
		return p[1]
	}
	return p[0]
}

// toward is how far v sits toward the phrase's end of the axis, 0..100.
func toward(v float64, high bool) int {
	if high {
		return pct(v)
	}
	return pct(1 - v)
}

func genreShares(w map[string]float64) map[string]float64 {
	var sum float64
	for _, x := range w {
		if x > 0 {
			sum += x
		}
	}
	out := make(map[string]float64, len(w))
	if sum == 0 {
		return out
	}
	for g, x := range w {
		if x > 0 {
			out[g] = x / sum
		}
	}
	return out
}

func sharedGenres(sa, sb map[string]float64) []string {
	var out []string
	for g, x := range sa {
		if x >= fusionGenreShare && sb[g] >= fusionGenreShare {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		mi, mj := math.Min(sa[out[i]], sb[out[i]]), math.Min(sa[out[j]], sb[out[j]])
		if mi != mj {
			return mi > mj
		}
		return out[i] < out[j]
	})
	return nonNil(out)
}

func shareText(s float64) string {
	switch {
	case s < 0.05:
		return "Almost never"
	case s < 0.20:
		return "Now and then"
	case s < 0.40:
		return "A good part of the shelf"
	default:
		return "Most of the shelf"
	}
}

func unionKeys(a, b map[string]float64) map[string]bool {
	out := make(map[string]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func pct(v float64) int { return int(math.Round(v * 100)) }

func roundTo(n, step int) int { return int(math.Round(float64(n)/float64(step))) * step }

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilBooks(s []FusionBook) []FusionBook {
	if s == nil {
		return []FusionBook{}
	}
	return s
}
