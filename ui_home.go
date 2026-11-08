package main

// 今日: one board of cells. Clicking a cell grows it in place while the
// others are squeezed into summaries that fit their new size (never
// truncated). Every view of a cell is laid out at the size the cell will
// have when the morph ends, so nothing reflows on the way; the cell's
// clip reveals it, and views come and go like drawers. Shared pieces
// (labels, the exit's name, the mode's underline) fly between views.

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/oboo/moho/internal/i18n"
)

type homeUI struct {
	x      string // the expanded cell, "" for none
	sub    string // a route whose node picker the routes cell shows
	region string // node picker filter: "", a country, or "?"
	query  string
	nodes  ui.ListState
	hist   []Tick // the last 90 seconds

	// What each cell showed in the last frame, and a view folding away.
	shown   map[string]shownView
	leaving map[string]leavingView

	// Where each shared piece's copy was, by cell and key (share), as of
	// the frame it was built: frame counts the board's frames.
	copies map[string]shareRec
	frame  int

	mapView mapView               // the expanded map's pan and zoom
	walls   map[string]*wallPaths // twisted borders' paths, by cell
	lines   map[string]*linePaths
}

type shownView struct {
	view  string
	size  ui.Rect
	since time.Time
}

type leavingView struct {
	shownView
	start time.Time
}

func newHomeUI() homeUI {
	return homeUI{shown: map[string]shownView{}, leaving: map[string]leavingView{}, copies: map[string]shareRec{}}
}

func (h *homeUI) pushTick(t Tick) {
	h.hist = append(h.hist, t)
	if len(h.hist) > 90 {
		h.hist = h.hist[len(h.hist)-90:]
	}
}

// Matches: identities shared between a cell's views.
type exitMatch struct{ route string }
type modeMatch struct{}
type entryMatch struct{ id string }
type routeNameMatch struct{ route string }
type speedMatch struct{}
type appMatch struct{ app string }

// A cell's title (its kicker) is one element for all its views, drawn by
// the cell above them: it moves with the cell's box as part of it, and
// eases to where each view puts it with a transition relative to the cell.
// Flying it between views instead chases a target the moving cell keeps
// moving, and lags. Views keep an invisible stand-in for their layout and
// say here where the title goes: kickAt is the offset of the next one,
// which pad, compact and detailHead set; cellKicker fills kickNow.
type kickSpot struct {
	text string
	x, y float32
}

var (
	kickAt  ui.Point
	kickNow *kickSpot
)

// noFly is set while a leaving view builds: its pieces must not claim the
// identities the arriving view flies with (one element per Match).
var noFly bool

// arriving is set while a view that just appeared builds. Only arriving
// pieces fly: a piece that flies from one that also flies would carry a
// fading copy of it along, a cross-fade, so pieces at rest do not fly.
var arriving bool

func fly(m any) ui.ElementTransition {
	if noFly {
		return ui.ElementTransition{Duration: -1}
	}
	return ui.ElementTransition{Animation: &motion, Match: m, Fly: arriving}
}

const foldOut = 200 * time.Millisecond

// flyAlpha hides shared pieces in a leaving view: their arriving twins
// are already flying, and each must show once.
func flyAlpha() float32 {
	if noFly {
		return 0
	}
	return 1
}

var morphText = ui.ContentTransition{Morph: true}

// Shared pieces, as the exit's name or an app's, are in several views of
// a cell. Like the cell's title, one element shows each on its way between
// views: a copy of it drawn by the cell over its views, which moves with
// the cell's box as part of it and eases to where the arriving view puts
// it, relative to the cell. A Match flying between the views would chase a
// target the moving cell keeps moving, and lag. The copy is built every
// frame, unseen at rest, so that its text morphs; the view's own piece is
// laid out in place, which says where the copy goes (LayoutBounds, of the
// frame before), and shows once the view has arrived.
type sharedPiece struct {
	key   string
	at    ui.Element // in place, in its view
	build func(c *ui.Context) ui.Element
}

type shareRec struct {
	rel   ui.Rect // relative to the view
	frame int
}

// shareTrace, if set, sees each copy on its way, and its piece in place,
// as of the frame before: for tests.
var shareTrace func(key string, copy, at ui.Rect)

var (
	shareCell string        // the cell being built, "" outside one
	shareLift bool          // the arriving view lifts its pieces
	shared    []sharedPiece // the arriving view's
	leftOver  []sharedPiece // the leaving view's
)

// share builds a piece of key, shared between the views of the cell, in
// place, and notes it for the cell to show its copy; m is its Match, with
// which it still flies elsewhere, as between the board's states.
func (u *UI) share(c *ui.Context, m any, key string, build func(c *ui.Context) ui.Element) ui.Element {
	e := build(c)
	switch {
	case shareCell == "":
		return e.Transition(fly(m))
	case noFly:
		leftOver = append(leftOver, sharedPiece{key: key, at: e})
		return e.Transition(fly(m))
	}
	shared = append(shared, sharedPiece{key: key, at: e, build: build})
	if shareLift && u.home.hadCopy(shareCell+"/"+key) {
		return e.Opacity(0) // its copy shows it on its way
	}
	return e.Transition(fly(m))
}

// steady returns was unless now is another place: an offset taken
// between two places in the window differs from frame to frame by a
// rounding error, and a target that changes, however little, starts its
// curve again (glide).
func steady(was, now ui.Rect) ui.Rect {
	const hair = 0.01
	if abs32(was.X-now.X) < hair && abs32(was.Y-now.Y) < hair && abs32(was.W-now.W) < hair && abs32(was.H-now.H) < hair {
		return was
	}
	return now
}

func abs32(f float32) float32 {
	if f < 0 {
		return -f
	}
	return f
}

// glide returns where the value name of e is on its way to v, easing
// there with motion, or there at once when !move. Pieces in a cell glide
// to their places in it rather than move with a Transition's Position,
// which heads for their place in the window, a target the moving cell
// keeps moving, restarting the curve every frame.
func glide(e ui.Element, name string, v float32, move bool) float32 {
	if !move {
		return e.Jump(name, v)
	}
	return e.AnimateTo(name, v, motion)
}

func (h *homeUI) hadCopy(k string) bool {
	r, ok := h.copies[k]
	return ok && r.frame == h.frame-1
}

// drawShared draws the copies of the pieces of the arriving view in, a
// child of the cell at its top left, and hides those of the leaving view
// that the arriving one has.
func (u *UI) drawShared(c *ui.Context, cell string, in ui.Element, lift bool) {
	h := &u.home
	keys := map[string]bool{}
	view := in.LayoutBounds()
	for _, s := range shared {
		keys[s.key] = true
		k := cell + "/" + s.key
		had := h.hadCopy(k)
		rel := h.copies[k].rel
		if b := s.at.LayoutBounds(); view.W > 0 && (b.W > 0 || b.H > 0) {
			rel = steady(rel, ui.Rect{X: b.X - view.X, Y: b.Y - view.Y, W: b.W, H: b.H})
		} else if !had {
			continue // no place for it yet: its piece shows in place
		}
		move := lift && had
		cp := s.build(c.Key("share-" + s.key))
		if shareTrace != nil && move {
			shareTrace(k, cp.LayoutBounds(), s.at.LayoutBounds())
		}
		// Only its place glides: laid out at its own size all the way, a
		// title never wraps in a box narrower than it on its way; the
		// cell may cut it off for a moment instead.
		x, y := glide(cp, "x", rel.X, move), glide(cp, "y", rel.Y, move)
		cp.Absolute().Left(x).Top(y).Size(rel.W, rel.H).Margin(0, 0, 0, 0).
			IgnoreInput(true).AccessHidden(true).Opacity(map[bool]float32{true: 1}[move])
		h.copies[k] = shareRec{rel: rel, frame: h.frame}
	}
	for _, s := range leftOver {
		if keys[s.key] {
			s.at.Opacity(0)
		}
	}
	shared, leftOver = shared[:0], leftOver[:0]
}

// ---- Layout ----

type cell struct {
	id   string
	r    int   // row 1–3
	cols []int // columns 1–3
}

var cells = []cell{
	{"hero", 1, []int{1}}, {"map", 1, []int{2, 3}},
	{"mode", 2, []int{1}}, {"takeover", 2, []int{2}}, {"routes", 2, []int{3}},
	{"speed", 3, []int{1}}, {"apps", 3, []int{2}}, {"events", 3, []int{3}},
}

const sqRow, sqCol = 78, 190

// tracks are the board's column widths and row heights (relative) for the
// state: normal, off (just the headline row), or a cell expanded.
func (u *UI) tracks(x string) (cols, rows [3]float32) {
	cols, rows = [3]float32{400, 330, 330}, [3]float32{318, 150, 218}
	if !u.snap.Takeover.On {
		return cols, [3]float32{686, 0, 0}
	}
	for _, c := range cells {
		if c.id != x {
			continue
		}
		switch c.r {
		case 1:
			rows = [3]float32{686 - 2*sqRow, sqRow, sqRow}
		case 2:
			rows = [3]float32{150, 686 - 150 - sqRow, sqRow}
		case 3:
			rows = [3]float32{150, sqRow, 686 - 150 - sqRow}
		}
		if len(c.cols) == 2 {
			cols = [3]float32{sqCol, (1060 - sqCol) / 2, (1060 - sqCol) / 2}
		} else {
			cols = [3]float32{sqCol, sqCol, sqCol}
			cols[c.cols[0]-1] = 1060 - 2*sqCol
		}
	}
	return cols, rows
}

// boxes turns tracks into cell rectangles for a board of w×h (1 DIP gaps).
func boxes(cols, rows [3]float32, w, h float32) map[string]ui.Rect {
	scale := func(a [3]float32, total float32) [3]float32 {
		sum := a[0] + a[1] + a[2]
		var out [3]float32
		for i := range a {
			out[i] = (total - 2) * a[i] / sum
		}
		return out
	}
	cw, rh := scale(cols, w), scale(rows, h)
	x := [3]float32{0, cw[0] + 1, cw[0] + cw[1] + 2}
	y := [3]float32{0, rh[0] + 1, rh[0] + rh[1] + 2}
	out := map[string]ui.Rect{}
	for _, c := range cells {
		r := ui.Rect{X: x[c.cols[0]-1], Y: y[c.r-1], H: rh[c.r-1]}
		for _, col := range c.cols {
			r.W += cw[col-1]
		}
		r.W += float32(len(c.cols) - 1)
		out[c.id] = r
	}
	return out
}

// variant is the summary a squeezed cell shows, chosen from its size.
func variant(r ui.Rect) string {
	switch {
	case r.H < 120 && r.W < 260:
		return "mini"
	case r.H < 120:
		return "strip"
	case r.W < 260:
		return "column"
	}
	return "wide"
}

// ---- The board ----

func (u *UI) homePage(c *ui.Context, p *pal) {
	h := &u.home
	if !u.snap.Takeover.On && h.x != "" || u.snap.External && (h.x == "mode" || h.x == "takeover") {
		h.x, h.sub = "", ""
	}
	c.OnWindowShortcut(0, ui.KeyEscape, func() {
		if h.x == "routes" && h.sub != "" {
			h.sub = ""
			return
		}
		h.x, h.sub = "", ""
	})
	// The lines between the cells are the board showing through the gaps,
	// or, twisted, drawn over them.
	twisted := u.shownSettings().Borders == "twisted"
	board := ui.Box(c).Grow(1).Background(p.hair).Clip()
	if twisted {
		board.Background(p.paper)
	} else {
		h.walls, h.lines = nil, nil
	}
	r := board.LayoutBounds()
	w, hh := r.W, r.H
	if w <= 0 {
		w, hh = 1060, 686
	}
	cols, rows := u.tracks(h.x)
	final := boxes(cols, rows, w, hh)
	ncols, nrows := u.tracks("")
	normal := boxes(ncols, nrows, w, hh)
	h.frame++
	for k, r := range h.copies {
		if r.frame < h.frame-1 {
			delete(h.copies, k)
		}
	}
	board.Children(func() {
		for _, cl := range cells {
			u.cell(c, p, cl, final[cl.id], normal[cl.id])
		}
		if !twisted {
			return
		}
		// Over the cells, as they are drawn on the cells' edges: the lines,
		// then the walls over them.
		for _, cl := range cells {
			h.line(c, p, cl.id, final[cl.id], w, hh)
		}
		for _, cl := range cells {
			h.wall(c, p, cl.id, final[cl.id], w, hh)
		}
	})
}

func (u *UI) cell(c *ui.Context, p *pal, cl cell, box, normalBox ui.Rect) {
	h := &u.home
	v := "normal"
	if h.x == cl.id {
		v = "expanded"
	} else if h.x != "" {
		v = "compact"
	}
	e := ui.ButtonBase(c.Key(cl.id)).Absolute().Left(box.X).Top(box.Y).Size(box.W, box.H).Clip().
		Background(p.paper).Transition(ui.ElementTransition{Animation: &motion, Colors: true, Position: true, Size: true})
	if v == "expanded" {
		e.Background(p.sheet).Cursor(ui.CursorDefault)
	} else if e.Hovered() && u.snap.Takeover.On {
		e.Background(p.paper.Mix(p.ink, .035))
	}
	if v != "expanded" && cl.id != "map" || cl.id == "map" && v != "expanded" {
		e.OnClick(func() {
			if u.snap.Takeover.On && !(u.snap.External && (cl.id == "mode" || cl.id == "takeover")) {
				u.home.x, u.home.sub, u.home.query, u.home.region = cl.id, "", "", ""
			}
		})
	} else if cl.id == "map" {
		e.OnClick(func() { u.home.x = "" })
	}
	e.Children(func() {
		if cl.id == "map" {
			u.mapCell(c, p, v, box)
			return
		}
		// One view at a time, each laid out at its final size, so nothing
		// reflows while the cell moves. The old view folds up like a
		// drawer at its own size; the new one opens top-down in place.
		shareCell = cl.id
		defer func() { shareCell = "" }()
		size := normalBox
		if v != "normal" {
			size = box
		}
		now := c.Now()
		if prev, ok := h.shown[cl.id]; ok && prev.view != v {
			h.leaving[cl.id] = leavingView{shownView: prev, start: now}
		}
		cur := h.shown[cl.id]
		if cur.view != v || cur.since.IsZero() {
			cur.since = now
		}
		h.shown[cl.id] = shownView{view: v, size: size, since: cur.since}
		if lv, ok := h.leaving[cl.id]; ok {
			if now.Sub(lv.start) < foldOut+50*time.Millisecond && lv.view != v {
				out := ui.Column(c.Key("leaving-"+lv.view)).Absolute().Left(0).Top(0).Size(lv.size.W, lv.size.H).IgnoreInput(true).AccessHidden(true)
				q := out.AnimateTo("fold", 0, ui.Animation{Duration: foldOut, Ease: ui.EaseIn})
				if now.Equal(lv.start) {
					out.Jump("fold", 1)
					q = out.AnimateTo("fold", 0, ui.Animation{Duration: foldOut, Ease: ui.EaseIn})
				}
				out.ClipInsetPercent(0, 0, (1-q)*100, 0, 0).Translate(0, -6*(1-q))
				noFly = true
				out.Children(func() { u.viewOf(c, p, cl.id, lv.view, lv.size) })
				noFly = false
				c.After(16 * time.Millisecond)
			} else {
				delete(h.leaving, cl.id)
			}
		}
		in := ui.Column(c.Key(v)).Absolute().Left(0).Top(0).Size(size.W, size.H)
		open := motion
		open.Delay = 110 * time.Millisecond
		reveal := in.AnimateTo("open", 1, open)
		if _, fresh := h.leaving[cl.id]; fresh && reveal == 1 {
			reveal = in.Jump("open", 0)
			reveal = in.AnimateTo("open", 1, open)
		}
		in.ClipInsetPercent(0, 0, (1-reveal)*100, 0, 0).Translate(0, -6*(1-reveal))
		arriving = now.Sub(h.shown[cl.id].since) < 700*time.Millisecond
		if arriving {
			c.After(100 * time.Millisecond) // rebuild once it has arrived
		}
		kickNow = nil
		lift := arriving
		shareLift = lift
		in.Children(func() { u.viewOf(c, p, cl.id, v, box) })
		arriving, shareLift = false, false
		u.drawShared(c, cl.id, in, lift)
		if k := kickNow; k != nil {
			kickNow = nil
			t := kicker(c.Key("kicker"), p, k.text)
			t.Absolute().Left(glide(t, "x", k.x, true)).Top(glide(t, "y", k.y, true)).NoWrap().IgnoreInput(true).ContentTransition(morphText)
		}
	})
}

// The focused cell's wall: its edge, drawn as the logo's line, thick and
// bending. Inside the line is the cell; outside, its neighbors. Edges on the
// board's border are drawn a little inside, so that the line shows whole.
const (
	wallWidth = 2   // the line, DIPs
	wallSwing = 2.6 // how far it bends to either side of the edge
	wallWave  = 46  // DIPs from one bulge to the next, about
	wallRound = 12  // the corners' radius
	wallInk   = .5  // how strong the line is, of the ink
	wallReach = wallSwing + wallWidth/2 + 1.5
)

// wall draws the wall of cell id, at box on a board w×h: it twists in as
// the cell takes the focus and untwists as it gives it up, its bulges
// rolling along the edge as they swell or settle.
func (h *homeUI) wall(c *ui.Context, p *pal, id string, box ui.Rect, w, hh float32) {
	m := float32(wallReach)
	e := ui.Box(c.Key("wall-"+id)).Absolute().Left(box.X-m).Top(box.Y-m).Size(box.W+2*m, box.H+2*m).
		IgnoreInput(true).AccessHidden(true).Transition(ui.ElementTransition{Animation: &motion, Position: true, Size: true})
	on := h.x == id
	twist := motion
	twist.Duration = 640 * time.Millisecond
	if on {
		twist.Delay = 110 * time.Millisecond
	}
	k := e.AnimateTo("twist", map[bool]float32{false: 0, true: 1}[on], twist)
	if k <= 0 {
		delete(h.walls, id)
		return
	}
	if h.walls == nil {
		h.walls = map[string]*wallPaths{}
	}
	wp := h.walls[id]
	if wp == nil {
		wp = &wallPaths{}
		h.walls[id] = wp
	}
	// Which sides are on the board's border, from the box at rest.
	left, top, right, bottom := box.X < .5, box.Y < .5, box.X+box.W > w-.5, box.Y+box.H > hh-.5
	e.Draw(func(pt *ui.Painter, r ui.Rect) {
		cell := ui.Rect{X: r.X + m, Y: r.Y + m, W: r.W - 2*m, H: r.H - 2*m}
		base := cell
		if left {
			base.X, base.W = base.X+m, base.W-m
		}
		if top {
			base.Y, base.H = base.Y+m, base.H-m
		}
		if right {
			base.W -= m
		}
		if bottom {
			base.H -= m
		}
		if key := [5]float32{base.X, base.Y, base.W, base.H, k}; key != wp.key || cell != wp.cell {
			wp.key, wp.cell = key, cell
			wp.build(cell, base, wallSwing*k, float64(1-k)*2.5*math.Pi)
		}
		// Side by side, so that each shape is a strip along an edge, not as
		// large as the cell: the shapes are rasterized anew as they change.
		// The line is opaque, so that where the sides meet doesn't darken.
		ink := p.sheet.Mix(p.ink, wallInk).Alpha(min(1, 2*k))
		for i := range 4 {
			pt.FillPath(&wp.out[i], p.paper.Alpha(k))
			pt.FillPath(&wp.in[i], p.sheet.Alpha(k))
		}
		for i := range 4 {
			pt.StrokePath(&wp.wall[i], wallWidth*(.5+.5*k), ink)
		}
	})
}

// wallPaths are a wall's paths by side (top, right, bottom, left), kept
// while it stays put: the board is drawn often, a wall seldom changes.
type wallPaths struct {
	key           [5]float32
	cell          ui.Rect
	wall, out, in [4]ui.Path
}

// build makes the wall round cell, bending about base: the line; the paper
// outside it, over the gap past the cell's edge and its line; and the sheet
// inside it, where it bulges past the cell's edge.
func (wp *wallPaths) build(cell, base ui.Rect, swing float32, roll float64) {
	line, cuts := wobblyFrame(base, wallRound, swing, wallWave, roll)
	n := len(line)
	if n == 0 {
		for i := range 4 {
			wp.wall[i].Reset()
			wp.out[i].Reset()
			wp.in[i].Reset()
		}
		return
	}
	reach := float32(lineReach)
	// The outer corners, clockwise from the top left's.
	outer := [4][2]float32{
		{cell.X - reach, cell.Y - reach}, {cell.X + cell.W + reach, cell.Y - reach},
		{cell.X + cell.W + reach, cell.Y + cell.H + reach}, {cell.X - reach, cell.Y + cell.H + reach},
	}
	// past puts q on the cell's edge on side i unless it is past it.
	past := func(i int, q [2]float32) ([2]float32, bool) {
		switch i {
		case 0:
			return [2]float32{q[0], min(q[1], cell.Y)}, q[1] < cell.Y
		case 1:
			return [2]float32{max(q[0], cell.X+cell.W), q[1]}, q[0] > cell.X+cell.W
		case 2:
			return [2]float32{q[0], max(q[1], cell.Y+cell.H)}, q[1] > cell.Y+cell.H
		}
		return [2]float32{min(q[0], cell.X), q[1]}, q[0] < cell.X
	}
	for i := range 4 {
		// Side i runs from the middle of the corner before it to that of
		// the corner after.
		from, to := cuts[(i+3)%4], cuts[i]
		if to < from {
			to += n
		}
		side := make([][2]float32, 0, to-from+1)
		for j := from; j <= to; j++ {
			side = append(side, line[j%n])
		}
		w := wp.wall[i].Reset()
		w.MoveTo(side[0][0], side[0][1])
		for j := 1; j < len(side)-1; j++ {
			w.QuadTo(side[j][0], side[j][1], (side[j][0]+side[j+1][0])/2, (side[j][1]+side[j+1][1])/2)
		}
		w.LineTo(side[len(side)-1][0], side[len(side)-1][1])
		// Outside: between the line, its bulges in, and the outer edge.
		o := wp.out[i].Reset()
		o.MoveTo(side[0][0], side[0][1])
		for _, q := range side[1:] {
			o.LineTo(q[0], q[1])
		}
		o.LineTo(outer[(i+1)%4][0], outer[(i+1)%4][1]).LineTo(outer[i][0], outer[i][1]).Close()
		// Inside, past the cell's edge: between its bulges out and the edge.
		in := wp.in[i].Reset()
		bulges := false
		for j, q := range side {
			e, out := past(i, q)
			bulges = bulges || out
			if j == 0 {
				in.MoveTo(e[0], e[1])
			} else {
				in.LineTo(e[0], e[1])
			}
		}
		if !bulges {
			in.Reset()
			continue
		}
		last, _ := past(i, side[len(side)-1])
		first, _ := past(i, side[0])
		// Back along the edge.
		if i%2 == 0 {
			in.LineTo(last[0], cellEdge(cell, i)).LineTo(first[0], cellEdge(cell, i))
		} else {
			in.LineTo(cellEdge(cell, i), last[1]).LineTo(cellEdge(cell, i), first[1])
		}
		in.Close()
	}
}

// cellEdge is where side i of cell is: its y for the top and bottom, its x
// for the right and left.
func cellEdge(cell ui.Rect, i int) float32 {
	switch i {
	case 0:
		return cell.Y
	case 1:
		return cell.X + cell.W
	case 2:
		return cell.Y + cell.H
	}
	return cell.X
}

// wobblyFrame is the rounded rectangle r, corners rad, its line swung swing
// to either side of the edge about every wave DIPs, as points eight a wave,
// clockwise from the top's left end; cuts are the indexes of the middles of
// the corners, clockwise from the top right's. The bulges go round a whole
// number of times, so the line meets itself smoothly; a slower second wave
// makes them unequal, as a drawn line's are. roll moves them along the
// edge, in radians of a wave.
func wobblyFrame(r ui.Rect, rad, swing, wave float32, roll float64) (pts [][2]float32, cuts [4]int) {
	rad = min(rad, r.W/2, r.H/2)
	if rad < 0 {
		return nil, cuts
	}
	sw, sh := float64(r.W-2*rad), float64(r.H-2*rad)
	R := float64(rad)
	arc := math.Pi / 2 * R
	total := 2*(sw+sh) + 4*arc
	if total <= 0 {
		return nil, cuts
	}
	k := math.Max(3, math.Round(total/float64(wave)))
	x0, y0 := float64(r.X), float64(r.Y)
	// at is the point s along the edge and the outward normal there.
	at := func(s float64) (x, y, nx, ny float64) {
		corner := func(cx, cy, from float64) (float64, float64, float64, float64) {
			a := from + s/R
			return cx + R*math.Cos(a), cy + R*math.Sin(a), math.Cos(a), math.Sin(a)
		}
		if s < sw {
			return x0 + R + s, y0, 0, -1
		}
		if s -= sw; s < arc {
			return corner(x0+R+sw, y0+R, -math.Pi/2)
		}
		if s -= arc; s < sh {
			return x0 + 2*R + sw, y0 + R + s, 1, 0
		}
		if s -= sh; s < arc {
			return corner(x0+R+sw, y0+R+sh, 0)
		}
		if s -= arc; s < sw {
			return x0 + R + sw - s, y0 + 2*R + sh, 0, 1
		}
		if s -= sw; s < arc {
			return corner(x0+R, y0+R+sh, math.Pi/2)
		}
		if s -= arc; s < sh {
			return x0, y0 + R + sh - s, -1, 0
		}
		s -= sh
		return corner(x0+R, y0+R, math.Pi)
	}
	n := int(k) * 8
	pts = make([][2]float32, n)
	for i := range pts {
		s := total * float64(i) / float64(n)
		x, y, nx, ny := at(s)
		t := 2 * math.Pi * s / total
		d := float64(swing) * (.8*math.Sin(k*t+roll) + .2*math.Sin(math.Round(k/3)*t+1-roll/2))
		pts[i] = [2]float32{float32(x + nx*d), float32(y + ny*d)}
	}
	for i, s := range [4]float64{sw + arc/2, sw + sh + arc*1.5, 2*sw + sh + arc*2.5, 2*sw + 2*sh + arc*3.5} {
		cuts[i] = int(math.Round(s/total*float64(n))) % n
	}
	return pts, cuts
}

// The twisted lines between the cells: thin, bending less, their bulges
// fixed to the board so that they stay put as the cells move.
const (
	lineSwing = 1.4
	lineReach = lineSwing + 2 // how far past a cell's edge its line goes
)

// line draws the twisted lines of cell id, at box on a board w×h: its right
// and bottom edges, unless they are the board's, so that each line between
// two cells is drawn once.
func (h *homeUI) line(c *ui.Context, p *pal, id string, box ui.Rect, w, hh float32) {
	left, top := box.X < .5, box.Y < .5
	right, bottom := box.X+box.W > w-.5, box.Y+box.H > hh-.5
	if right && bottom {
		return
	}
	if h.lines == nil {
		h.lines = map[string]*linePaths{}
	}
	lp := h.lines[id]
	if lp == nil {
		lp = &linePaths{}
		h.lines[id] = lp
	}
	m := float32(lineReach)
	col := p.hair.Over(p.paper) // opaque, so that lines crossing don't darken
	ui.Box(c.Key("line-"+id)).Absolute().Left(box.X-m).Top(box.Y-m).Size(box.W+2*m, box.H+2*m).
		IgnoreInput(true).AccessHidden(true).Transition(ui.ElementTransition{Animation: &motion, Position: true, Size: true}).
		Draw(func(pt *ui.Painter, r ui.Rect) {
			cell := ui.Rect{X: r.X + m, Y: r.Y + m, W: r.W - 2*m, H: r.H - 2*m}
			if cell != lp.cell {
				lp.cell = cell
				lp.path.Reset()
				// The gaps' middles round the cell. A line ends where it meets
				// the line across it, which bends about the gap too: both lines
				// pass there, so they join. At the board's border it runs off.
				lx, ty := cell.X-.5, cell.Y-.5
				rx, by := cell.X+cell.W+.5, cell.Y+cell.H+.5
				meet := func(x, y float32) [2]float32 { return [2]float32{x + lineBend(y), y + lineBend(x)} }
				off := func(x, y float32, down bool) [2]float32 {
					if down {
						return [2]float32{x + lineBend(y), y}
					}
					return [2]float32{x, y + lineBend(x)}
				}
				if !right {
					from, to := meet(rx, ty), meet(rx, by)
					if top {
						from = off(rx, cell.Y-m, true)
					}
					if bottom {
						to = off(rx, cell.Y+cell.H+m, true)
					}
					wavyLine(&lp.path, rx, from, to, true)
				}
				if !bottom {
					from, to := meet(lx, by), meet(rx, by)
					if left {
						from = off(cell.X-m, by, false)
					}
					if right {
						to = off(cell.X+cell.W+m, by, false)
					}
					wavyLine(&lp.path, by, from, to, false)
				}
			}
			pt.StrokePath(&lp.path, 1, col)
		})
}

// linePaths are a cell's twisted lines, kept while it stays put.
type linePaths struct {
	cell ui.Rect
	path ui.Path
}

// lineBend is how far a twisted line at t along the board's x (or y) bends
// from its gap: the bulges are the board's, not the line's, so that a line
// moved along keeps them where they were, and lines that cross meet.
func lineBend(t float32) float32 {
	u := 2 * math.Pi * float64(t) / wallWave
	return lineSwing * float32(.8*math.Sin(u)+.2*math.Sin(u/3+1))
}

// wavyLine adds to path the twisted line of the gap at gap along the
// board's y (or, down, its x), from a to b: across the board, or down it.
func wavyLine(path *ui.Path, gap float32, a, b [2]float32, down bool) {
	along := func(q [2]float32) float32 {
		if down {
			return q[1]
		}
		return q[0]
	}
	at := func(t float32) [2]float32 {
		if down {
			return [2]float32{gap + lineBend(t), t}
		}
		return [2]float32{t, gap + lineBend(t)}
	}
	pts := [][2]float32{a}
	step := float32(wallWave) / 8
	ta, tb := along(a), along(b)
	for t := float32(math.Floor(float64(ta/step))+1) * step; t < tb-step/4; t += step {
		pts = append(pts, at(t))
	}
	pts = append(pts, b)
	path.MoveTo(pts[0][0], pts[0][1])
	for i := 1; i < len(pts)-1; i++ {
		path.QuadTo(pts[i][0], pts[i][1], (pts[i][0]+pts[i+1][0])/2, (pts[i][1]+pts[i+1][1])/2)
	}
	path.LineTo(b[0], b[1])
}

func (u *UI) viewOf(c *ui.Context, p *pal, id, v string, box ui.Rect) {
	if u.snap.External && (id == "mode" || id == "takeover") {
		u.coreCell(c, p, id, v)
		return
	}
	switch v {
	case "normal":
		u.normalView(c, p, id)
	case "compact":
		u.compactView(c, p, id, variant(box))
	default:
		u.expandedView(c, p, id, box.W)
	}
}

func (u *UI) normalView(c *ui.Context, p *pal, id string) {
	switch id {
	case "hero":
		u.heroNormal(c, p)
	case "mode":
		u.modeNormal(c, p)
	case "takeover":
		u.takeoverNormal(c, p)
	case "routes":
		u.routesNormal(c, p)
	case "speed":
		u.speedNormal(c, p)
	case "apps":
		u.appsNormal(c, p)
	case "events":
		u.eventsNormal(c, p)
	}
}

func (u *UI) expandedView(c *ui.Context, p *pal, id string, w float32) {
	switch id {
	case "hero":
		u.heroExpanded(c, p, w)
	case "mode":
		u.modeExpanded(c, p)
	case "takeover":
		u.takeoverExpanded(c, p)
	case "routes":
		u.routesExpanded(c, p, w)
	case "speed":
		u.speedExpanded(c, p)
	case "apps":
		u.appsExpanded(c, p)
	case "events":
		u.eventsExpanded(c, p)
	}
}

// pad is the padding of a normal view.
func pad(c *ui.Context, top float32) ui.Element {
	kickAt = ui.Point{X: 26, Y: top}
	return ui.Column(c).Fill().Padding(top, 26, 18, 26)
}

// cellKicker is the stand-in for the cell's title, at kickAt in its view.
func cellKicker(c *ui.Context, p *pal, cell, text string) ui.Element {
	if !noFly {
		kickNow = &kickSpot{text: text, x: kickAt.X, y: kickAt.Y}
	}
	return kicker(c, p, text).Opacity(0).AccessHidden(true)
}

// detailHead is the header of an expanded view: its label, a title, controls, and ×.
func (u *UI) detailHead(c *ui.Context, p *pal, cell, label string, title func(), right func()) {
	kickAt = ui.Point{X: 26, Y: 16}
	ui.Row(c).Gap(14).Padding(16, 26, 12, 26).BorderWidth(0, 0, 1, 0).BorderColor(p.hair).Children(func() {
		ui.Column(c).Gap(4).Grow(1).Children(func() {
			cellKicker(c, p, cell, label)
			title()
		})
		if right != nil {
			right()
		}
		iconBtn(c.Key("collapse"), p, "×", 18, i18n.T("收起", "Collapse")).Margin(-4, -6, -4, 0).OnClick(func() { u.home.x, u.home.sub = "", "" })
	})
}

func detailTitle(c *ui.Context, p *pal, s string) ui.Element {
	return disp(ui.Text(c, s), p).FontSize(22).FontWeight(700).TextColor(p.ink)
}

// ---- Compact summaries ----

// compact is a squeezed cell: a label, the value, and only what fits.
func compact(c *ui.Context, p *pal, v, cell, label string, value func(size float32), sub string, list func()) {
	kickAt = ui.Point{X: 18, Y: 12}
	if v == "strip" {
		kickAt.Y += 4
	}
	ui.Column(c).Fill().Padding(12, 18).Gap(5).Children(func() {
		k := cellKicker(c, p, cell, label)
		if v == "strip" {
			k.Margin(4, 0, 0, 0)
		}
		size := float32(17)
		if v == "wide" {
			size = 24
		}
		ui.Row(c).Gap(10).AlignItems(ui.Baseline).Children(func() {
			value(size)
			if sub != "" && v == "strip" {
				ui.Text(c, sub).FontSize(11.5).TextColor(p.mute).NoWrap()
			}
		})
		if sub != "" && (v == "column" || v == "wide") {
			ui.Text(c, sub).FontSize(11.5).TextColor(p.mute).MaxLines(2).Ellipsis("…")
		}
		if list != nil && (v == "column" || v == "wide") {
			ui.Column(c).Gap(8).Margin(8, 0, 0, 0).Children(list)
		}
	})
}

func (u *UI) compactView(c *ui.Context, p *pal, id, v string) {
	s := u.snap
	switch id {
	case "hero":
		ex, ok := u.exitNode()
		val, flag := i18n.T("未接管", "Not taken over"), false
		if s.Mode == "direct" {
			val = i18n.T("直接连接", "Direct")
		} else if ok {
			val, flag = ex.Display, true
		} else if len(s.Nodes) == 0 {
			val = i18n.T("还没有节点", "No nodes yet")
		}
		sub := ""
		if ok {
			sub = fmt.Sprintf("%s · %s · %s", fmtDelay(ex), placeName(ex), ex.Type)
		}
		compact(c, p, v, id, i18n.T("当前出口", "Current exit"), func(size float32) {
			u.share(c, exitMatch{"default"}, "exit", func(c *ui.Context) ui.Element {
				text := func() ui.Element {
					t := disp(ui.Text(c, val), p).FontSize(size).FontWeight(600).TextColor(p.ink).ContentTransition(morphText)
					if v == "mini" || v == "strip" {
						t.NoWrap()
					} else {
						t.MaxLines(2).Ellipsis("…").Tooltip(val)
					}
					return shimmer(t, u.pending("node:default"), p)
				}
				if flag {
					return flagged(c, p, ex, size, func() { text() })
				}
				return text()
			})
		}, sub, func() {
			if ok {
				if !s.External {
					ui.Text(c, i18n.T("来自 ", "From ")+u.providerName(ex.Provider)).FontSize(12).TextColor(p.ink2).MaxLines(1).Ellipsis("…")
				}
				mode := i18n.T("手动选择", "Manual")
				if u.route("default").Auto {
					mode = i18n.T("自动选最快", "Auto-pick fastest")
				}
				ui.Text(c, mode).FontSize(12).TextColor(p.ink2)
			}
		})
	case "mode":
		compact(c, p, v, id, i18n.T("模式", "Mode"), func(size float32) {
			disp(ui.Text(c, modeWord(s.Mode)), p).FontSize(size).FontWeight(600).TextColor(p.ink)
		}, modeNote(s.Mode), nil)
	case "takeover":
		names := u.entryNames()
		val, sub := i18n.T("未接管", "Not taken over"), ""
		if len(names) > 0 {
			val, sub = strings.Join(names, " + "), i18n.T("生效中", "Active")
		}
		col := p.ink
		if s.Takeover.System.Status == "hijacked" {
			col, sub = p.signal, i18n.T("被其他程序改掉了", "Changed by another app")
		}
		compact(c, p, v, id, i18n.T("接管", "Takeover"), func(size float32) {
			t := dispIn(ui.Text(c, val), p, col).FontSize(size).FontWeight(600).TextColor(col)
			if v == "mini" || v == "strip" {
				t.NoWrap()
			} else {
				t.MaxLines(2).Ellipsis("…")
			}
			shimmer(t, u.pending("entry:sys") || u.pending("entry:tun"), p)
		}, sub, func() {
			ui.Text(c, fmt.Sprintf(i18n.T("端口 %d", "Port %d"), s.Takeover.Port)).FontSize(12).TextColor(p.ink2)
			lan := i18n.T("不允许", "Not allowed")
			if s.Takeover.Lan {
				lan = i18n.T("允许", "Allowed")
			}
			ui.Text(c, i18n.T("局域网 ", "LAN ")+lan).FontSize(12).TextColor(p.ink2)
			if s.Takeover.Gateway.Wanted {
				_, st := u.gatewayStatus(p)
				ui.Text(c, i18n.T("网关 · ", "Gateway · ")+st).FontSize(12).TextColor(p.ink2).MaxLines(2).Ellipsis("…")
			}
		})
	case "routes":
		var proxied []RouteView
		for _, r := range s.Routes {
			if r.ID != "default" && !r.Direct {
				proxied = append(proxied, r)
			}
		}
		if v == "strip" {
			compact(c, p, v, id, i18n.T("其他路由", "Other routes"), func(float32) {
				ui.Row(c).Gap(14).Children(func() {
					for _, r := range proxied {
						ui.Row(c).Gap(5).Children(func() {
							swatch(c, p.routeColor(r.ID), 8)
							name := i18n.T("直接连接", "Direct")
							if n, ok := u.node(r.Node); ok {
								name = n.Display
							}
							ui.Text(c, r.Name+" → "+name).FontSize(12.5).TextColor(p.ink2).NoWrap()
						})
					}
				})
			}, "", nil)
			return
		}
		compact(c, p, v, id, i18n.T("其他路由", "Other routes"), func(float32) {
			ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
				for _, r := range proxied {
					swatch(c, p.routeColor(r.ID), 8)
				}
				ui.Text(c, fmt.Sprintf(i18n.T("%d 条", "%d"), len(proxied))).FontSize(12).TextColor(p.mute).Margin(0, 0, 0, 4)
			})
		}, "", func() {
			for _, r := range proxied {
				ui.Column(c).Gap(2).Children(func() {
					ui.Row(c).Gap(5).Children(func() {
						swatch(c, p.routeColor(r.ID), 8)
						ui.Text(c, r.Name).FontSize(11.5).TextColor(p.ink2)
					})
					if n, ok := u.node(r.Node); ok {
						flagged(c, p, n, 14, func() {
							ui.Text(c, n.Display).FontSize(14).FontWeight(600).TextColor(p.ink)
						})
					}
				})
			}
		})
	case "speed":
		compact(c, p, v, id, i18n.T("此刻速度", "Speed now"), func(size float32) {
			u.share(c, speedMatch{}, "speed", func(c *ui.Context) ui.Element {
				return disp(ui.Text(c, "↓ "+fmtRate(u.tick.Down)), p).FontSize(size).FontWeight(600).TextColor(p.ink).FontFeatures("tnum").NoWrap()
			})
		}, fmt.Sprintf(i18n.T("↑ %s · 今日 %s", "↑ %s · Today %s"), fmtRate(u.tick.Up), fmtBytes(u.tick.TodayDown)), nil)
	case "apps":
		top := "—"
		sub := fmt.Sprintf(i18n.T("共 %d 个连接", "%d connections"), u.tick.Active)
		if len(u.tick.Apps) > 0 {
			top = u.tick.Apps[0].App
			sub = fmtRate(u.tick.Apps[0].Down) + " · " + sub
		}
		compact(c, p, v, id, i18n.T("谁在用", "Who's using it"), func(size float32) {
			u.share(c, appMatch{top}, "app:"+top, func(c *ui.Context) ui.Element {
				return disp(ui.Text(c, top), p).FontSize(size).FontWeight(600).TextColor(p.ink).NoWrap()
			})
		}, sub, func() {
			for i, a := range u.tick.Apps {
				if i == 0 || i > 3 {
					continue
				}
				ui.Row(c).Gap(8).Children(func() {
					ui.Text(c, a.App).FontSize(12).TextColor(p.ink2)
					ui.Text(c, fmtRate(a.Down)).FontSize(12).TextColor(p.mute).FontFeatures("tnum")
				})
			}
		})
	case "events":
		latest, route := "", ""
		if len(u.tick.Events) > 0 {
			latest, route = u.tick.Events[0].Text, u.route(u.tick.Events[0].Route).Name
		}
		if v == "strip" {
			compact(c, p, v, id, i18n.T("刚刚", "Just now"), func(float32) {
				ui.Text(c, latest).FontSize(12.5).TextColor(p.ink2).NoWrap()
			}, route, nil)
			return
		}
		compact(c, p, v, id, i18n.T("刚刚", "Just now"), func(size float32) {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Box(c).Size(7, 7).Radius(p.r(4)).Background(p.good)
				disp(ui.Text(c, i18n.T("实时", "Live")), p).FontSize(size).FontWeight(600).TextColor(p.ink)
			})
		}, "", func() {
			for i, ev := range u.tick.Events {
				if i >= 4 {
					break
				}
				ui.Text(c, ev.Text).FontSize(12).TextColor(p.ink2)
			}
		})
	}
}

// ---- Hero: the exit ----

// exitNode is the node the default route uses now, if traffic goes through one.
func (u *UI) exitNode() (NodeView, bool) {
	if !u.snap.Takeover.On || u.snap.Mode == "direct" || u.snap.Exit == nil {
		return NodeView{}, false
	}
	return u.node(u.snap.Exit.Node)
}

func (u *UI) heroNormal(c *ui.Context, p *pal) {
	s := u.snap
	if !s.Takeover.On {
		u.heroOff(c, p)
		return
	}
	ex, ok := u.exitNode()
	switching := u.pending("node:default")
	col := pad(c, 24)
	room := col.LayoutBounds().W - 52 // its padding
	// A short cell (a window near its least height) keeps the note to one
	// line, or the latency below rides up over it.
	h := col.LayoutBounds().H
	tight := h > 0 && h < 300
	col.Children(func() {
		label := i18n.T("当前出口 · 默认路由", "Current exit · Default route")
		if s.Mode == "global" {
			label = i18n.T("当前出口 · 全局", "Current exit · Global")
		}
		cellKicker(c, p, "hero", label)
		headline := i18n.T("直接连接", "Direct")
		switch {
		case ok:
			headline = ex.Display
		case len(s.Nodes) == 0 && s.Mode != "direct":
			headline = i18n.T("还没有节点", "No nodes yet")
		}
		size := heroSize(c, headline, room, ok)
		u.share(c, exitMatch{"default"}, "exit", func(c *ui.Context) ui.Element {
			text := func() ui.Element {
				return shimmer(disp(ui.Text(c, headline), p).FontSize(size).FontWeight(700).LetterSpacing(-.8*size/heroBig).TextColor(p.ink).
					MaxLines(2).ContentTransition(morphText), switching, p)
			}
			if ok {
				return flagged(c, p, ex, size, func() { text().Grow(1).Basis(0) }).Margin(12, 0, 0, 0)
			}
			return text().Margin(12, 0, 0, 0)
		})

		ui.Box(c).Margin(10, 0, 0, 0).MinHeight(44).Children(func() {
			switch {
			case switching:
				shimmer(ui.Text(c, i18n.T("正在切换并测速…", "Switching and testing…")).FontSize(13.5).TextColor(p.ink2), true, p)
			case ok:
				ui.Column(c).Gap(4).Children(func() {
					// One line: a long provider name ends in "…" rather than
					// pushing the place and its tag into a column of letters.
					ui.Row(c).Gap(6).AlignItems(ui.Center).MinWidth(0).Children(func() {
						ui.Text(c, placeName(ex)).FontSize(13.5).TextColor(p.ink2).NoWrap().Shrink(0)
						locTag(c, p, ex.Where.Source)
						lead := "· "
						if ex.Type != "" {
							lead += ex.Type + " · "
						}
						if s.External { // its nodes have no provider
							ui.Text(c, strings.TrimSuffix(strings.TrimSpace(lead), "·")).FontSize(13.5).TextColor(p.ink2)
							return
						}
						ui.RichText(c, ui.Span{Text: lead + i18n.T("来自 ", "From "), Size: 13.5, Color: p.ink2},
							ui.Span{Text: u.providerName(ex.Provider), Size: 13.5, Weight: 600, Color: p.ink}).
							MaxLines(1).Ellipsis("…").Shrink(1).MinWidth(0).Tooltip(u.providerName(ex.Provider))
					})
					note := fmt.Sprintf(i18n.T("没命中其他规则的流量从这里出去，约占 %d%%。", "Traffic no other rule matches leaves through here, about %d%%."), int(u.route("default").Share*100+.5))
					if s.Mode == "global" {
						note = i18n.T("全局模式下，所有流量都从这里出去。", "In global mode, all traffic leaves through here.")
					}
					t := ui.Text(c, note).FontSize(13.5).TextColor(p.ink2)
					if tight {
						t.MaxLines(1).Ellipsis("…").Tooltip(note)
					}
				})
			case s.Mode == "direct":
				ui.Text(c, i18n.T("直连模式：所有流量不经过任何节点，规则暂停。适合排查网络问题。", "Direct mode: no traffic goes through any node and rules are paused. Useful for troubleshooting the network.")).FontSize(13.5).TextColor(p.ink2)
			case s.External:
				ui.Text(c, i18n.T("这个内核里还没有节点。", "The core has no nodes yet.")).FontSize(13.5).TextColor(p.ink2)
			default:
				ui.Text(c, i18n.T("添加一个提供商（订阅链接、节点链接或配置文件），节点就会出现在这里。现在所有流量都在直接连接。", "Add a provider (a subscription link, node link or config file) and its nodes show up here. For now all traffic connects directly.")).FontSize(13.5).TextColor(p.ink2)
			}
		})
		ui.Spacer(c)
		if ok {
			ui.Row(c).Gap(18).AlignItems(ui.Baseline).Children(func() {
				if !ex.Tested {
					// A dash this big reads as a stray bar: say it in words.
					ui.Text(c, i18n.T("还没测速", "Not tested yet")).FontSize(15).FontWeight(500).TextColor(p.mute)
					return
				}
				ui.Row(c).AlignItems(ui.Baseline).Gap(3).Children(func() {
					d, size := fmt.Sprint(ex.Delay), float32(38)
					if ex.Delay <= 0 {
						// A word, not a number: smaller, so that "タイムアウト"
						// stays on one line beside the bars.
						d, size = i18n.T("超时", "Timeout"), 26
					}
					disp(ui.Text(c, d), p).FontSize(size).NoWrap().FontWeight(600).LetterSpacing(-1).FontFeatures("tnum").TextColor(delayColor(p, ex.Delay, ex.Tested)).ContentTransition(morphText)
					if ex.Delay > 0 {
						ui.Text(c, "ms").FontSize(14).FontWeight(500).TextColor(p.mute)
					}
				})
				if len(ex.History) > 0 {
					ui.Column(c).Gap(4).Children(func() {
						ui.Text(c, fmt.Sprintf(i18n.T("最近 %d 次测速", "Last %d tests"), len(ex.History))).FontSize(10.5).LetterSpacing(.6).TextColor(p.mute).NoWrap()
						stabilityBars(c, p, ex.History, 5, 20)
					})
				}
			})
			ui.Row(c).Gap(10).Margin(16, 0, 0, 0).Children(func() {
				pill(c, p, i18n.T("换节点", "Change node"), true).OnClick(func() { u.home.x = "hero" })
				def := u.route("default")
				if s.External {
					return
				}
				g := pill(c, p, i18n.T("自动选最快", "Auto-pick fastest"), false)
				if def.Auto {
					g.Border(1.5, p.ink)
				}
				g.OnClick(func() {
					if !def.Auto {
						u.do(func() error { return u.app.selectNode("default", "") })
					}
				})
			})
		} else if len(s.Nodes) == 0 && s.Mode != "direct" && !s.External {
			pill(c, p, i18n.T("添加提供商", "Add provider"), true).AlignSelf(ui.Start).OnClick(func() { u.page = "providers" })
		}
	})
}

const heroBig, heroSmall = 46, 30

// heroSize is the largest size, from heroBig down to heroSmall, at which
// the hero's headline fits on one line in room DIPs (with its flag when
// flag): a long node name such as "Origin-Hysteria2-443" otherwise breaks
// at its hyphens and runs past the cell. Below heroSmall it wraps instead.
// room is the previous frame's; before the first, the headline is big.
func heroSize(c *ui.Context, s string, room float32, flag bool) float32 {
	if room <= 0 {
		return heroBig
	}
	for size := float32(heroBig); size > heroSmall; size -= 2 {
		r := room
		if flag { // as flagged lays it out
			r -= round(round(size*.62)*4/3) + round(size*.36)
		}
		w, _ := c.MeasureText(0, ui.Span{Text: s, Size: size, Weight: 700})
		w -= .8 * size / heroBig * float32(len([]rune(s))) // its letter spacing
		if w <= r {
			return size
		}
	}
	return heroSmall
}

// heroOff: the whole board is the headline and the two ways in.
func (u *UI) heroOff(c *ui.Context, p *pal) {
	ui.Column(c).Fill().Padding(0, 56).Justify(ui.Center).Children(func() {
		kicker(c, p, "MOHO")
		disp(ui.Text(c, i18n.T("未接管", "Not taken over")), p).FontSize(64).FontWeight(700).LetterSpacing(-1.2).TextColor(p.ink).Margin(14, 0, 0, 0).
			Transition(fly(exitMatch{"default"})).ContentTransition(morphText)
		ui.Text(c, i18n.T("所有应用都直接连接互联网，流量不经过 bowel。", "Every app connects to the internet directly; no traffic goes through bowel.")).FontSize(15).TextColor(p.ink2).Margin(12, 0, 0, 0)
		ui.Column(c).Gap(10).Margin(34, 0, 0, 0).MaxWidth(360).Children(func() {
			u.choice(c, p, "sys", i18n.T("系统代理", "System proxy"), i18n.T("浏览器和大多数应用 · 推荐", "Browsers and most apps · Recommended"))
			install := i18n.T("所有应用 · 首次需要授权", "All apps · Needs authorization the first time")
			if u.snap.Takeover.Tun.Status != "install" {
				install = i18n.T("所有应用，包括不认系统代理的", "All apps, including those that ignore the system proxy")
			}
			u.choice(c, p, "tun", "TUN", install)
		})
		if len(u.snap.Providers) == 0 {
			ui.Text(c, i18n.T("还没有提供商：接管后可以在「提供商」里添加。", "No providers yet: after taking over, add one under Provider.")).FontSize(11.5).TextColor(p.mute).Margin(14, 0, 0, 0)
		}
	})
}

func (u *UI) choice(c *ui.Context, p *pal, id, word, desc string) {
	busy := u.pending("entry:" + id)
	b := ui.ButtonBase(c.Key(id)).Column().AlignItems(ui.Start).Padding(14, 18).Radius(p.r(12)).Border(1, p.hair).Gap(3)
	if b.Hovered() || busy {
		b.Border(1.5, p.ink).Background(p.card)
	}
	b.Transition(ui.ElementTransition{Colors: true, Duration: 150 * time.Millisecond})
	b.OnClick(func() {
		if id == "sys" {
			u.do(func() error { return u.app.setSystemProxy(true) })
		} else {
			u.do(func() error { return u.app.setTun(true) })
		}
	}).Children(func() {
		shimmer(disp(ui.Text(c, word), p).FontSize(22).FontWeight(700).TextColor(p.ink).Opacity(flyAlpha()).Transition(fly(entryMatch{id})), busy, p)
		if busy {
			msg := i18n.T("正在接管…", "Taking over…")
			if id == "tun" && u.snap.Takeover.Tun.Status == "install" {
				msg = i18n.T("正在安装，请在系统弹窗中确认…", "Installing; confirm in the system dialog…")
			}
			ui.Text(c, msg).FontSize(12).TextColor(p.mute)
		} else {
			ui.Text(c, desc).FontSize(12).TextColor(p.mute)
		}
	})
}

func stabilityBars(c *ui.Context, p *pal, hist []int, w, h float32) ui.Element {
	return stabilityBarsIn(c, p, p.ink.Alpha(.7), hist, w, h)
}

func stabilityBarsIn(c *ui.Context, p *pal, ink ui.Color, hist []int, w, h float32) ui.Element {
	n := len(hist)
	return ui.Box(c).Size(float32(max(n, 1))*(w+3), h).Draw(func(pt *ui.Painter, r ui.Rect) {
		for i, d := range hist {
			bh := h
			col := p.signal
			if d > 0 {
				bh = max(3, h-float32(d)/7)
				col = ink
			}
			x := r.X + float32(i)*(w+3)
			pt.Fill(ui.Rect{X: x, Y: r.Y + h - bh, W: w, H: bh}, col, 0)
		}
	})
}

func (u *UI) heroExpanded(c *ui.Context, p *pal, w float32) {
	ex, ok := u.exitNode()
	u.detailHead(c, p, "hero", i18n.T("当前出口 · 默认路由 · 选择出口", "Current exit · Default route · Choose exit"), func() {
		u.share(c, exitMatch{"default"}, "exit", func(c *ui.Context) ui.Element {
			if !ok {
				return detailTitle(c, p, i18n.T("选择出口", "Choose exit"))
			}
			return flagged(c, p, ex, 22, func() {
				shimmer(detailTitle(c, p, ex.Display).MaxLines(2).Ellipsis("…").Tooltip(ex.Display).ContentTransition(morphText), u.pending("node:default"), p)
			})
		})
	}, func() { u.autoSeg(c, p, "default") })
	u.nodePicker(c, p, "default", w)
}

// autoSeg chooses automatic or manual for a route.
func (u *UI) autoSeg(c *ui.Context, p *pal, rid string) {
	if u.snap.External {
		return
	}
	r := u.route(rid)
	ui.Row(c).Padding(2).Radius(p.r(8)).Background(p.hair2).Children(func() {
		for _, opt := range []struct {
			label string
			auto  bool
		}{{i18n.T("自动", "Auto"), true}, {i18n.T("手动", "Manual"), false}} {
			on := r.Auto == opt.auto
			b := ui.ButtonBase(c.Key(fmt.Sprint(opt.auto))).Padding(3, 11).Radius(p.r(6))
			if on {
				b.Background(p.card).Shadow(0, 1, 2, 0, ui.RGBA(0, 0, 0, .12))
			}
			b.OnClick(func() {
				if opt.auto && !r.Auto {
					u.do(func() error { return u.app.selectNode(rid, "") })
				}
			}).Children(func() {
				w := 500
				if on {
					w = 600
				}
				ui.Text(c, opt.label).FontSize(12).FontWeight(w).TextColor(p.ink)
			})
		}
	})
}

// The region bar's measures, shared by its layout and peekWidth's sums.
const (
	regionBarPad   = 16
	regionBarGap   = 14
	regionChipPadX = 10
	regionChipGap  = 2
	regionFlag     = 14
	regionFlagGap  = 6
	regionSearchW  = 180
)

// peekWidth is how wide a strip of items widths wide, gap apart, is in
// avail DIPs. cut is false when they all fit. Otherwise the strip ends in
// the middle of the last item whose first half fits, so that half shows.
func peekWidth(widths []float32, gap, avail float32) (w float32, cut bool) {
	var total float32
	for i, iw := range widths {
		if i > 0 {
			total += gap
		}
		total += iw
	}
	// Too narrow for even half the first item: let the strip shrink.
	if total <= avail || len(widths) == 0 || avail < widths[0]/2 {
		return 0, false
	}
	var x float32
	for _, iw := range widths {
		if mid := x + iw/2; mid <= avail {
			w = mid
		} else {
			break
		}
		x += iw + gap
	}
	return w, true
}

// nodePicker lists every node a route can use, fastest first.
func (u *UI) nodePicker(c *ui.Context, p *pal, rid string, w float32) {
	h := &u.home
	r := u.route(rid)
	testing := u.pending("test:" + rid)
	countries := map[string]string{}
	var order []string
	for _, n := range u.snap.Nodes {
		if pl := n.Where.Place; pl != nil {
			if _, seen := countries[pl.Country]; !seen {
				countries[pl.Country] = pl.Flag
				order = append(order, pl.Country)
			}
		}
	}
	sort.Strings(order)
	type region struct{ id, flag, label string }
	regions := []region{{"", "-", i18n.T("全部", "All")}}
	for _, cc := range order {
		regions = append(regions, region{cc, countries[cc], cc})
	}
	regions = append(regions, region{"?", "", i18n.T("未知", "Unknown")})
	label := i18n.T("全部测速", "Test all")
	if testing {
		label = i18n.T("测速中…", "Testing…")
	}
	// The bar's height leaves the chips 12 DIPs above and below, so the
	// strip's scroll bar (3–9 DIPs up from its bottom) runs under them
	// instead of across their labels.
	ui.Row(c).Gap(regionBarGap).Padding(4, regionBarPad).BorderWidth(0, 0, 1, 0).BorderColor(p.hair2).AlignItems(ui.Center).Children(func() {
		// Each region is a pill: the same round flag the node rows show,
		// then its letters; the chosen one sits on a faint fill.
		chip := func(r region) {
			on := h.region == r.id
			b := ui.ButtonBase(c.Key("r"+r.id)).Row().AlignItems(ui.Center).Gap(regionFlagGap).Padding(4, regionChipPadX).Radius(p.r(99))
			switch {
			case on:
				b.Background(p.hair2)
			case b.Hovered():
				b.Background(p.hover)
			}
			b.OnClick(func() { h.region = r.id }).Children(func() {
				col, wgt := p.mute, 500
				if on {
					col, wgt = p.ink, 600
				}
				if r.flag != "-" {
					flagBadge(c, p, r.flag, regionFlag)
				}
				ui.Text(c, r.label).FontSize(12).FontWeight(wgt).TextColor(col).NoWrap()
			})
		}
		// Many regions scroll sideways, so the search and 全部测速 stay.
		// When they overflow, the strip ends halfway through a chip: the
		// cut-off half says there is more to scroll to, where a strip that
		// happened to end between two chips would look complete.
		widths := make([]float32, len(regions))
		for i, r := range regions {
			wgt := 500
			if h.region == r.id {
				wgt = 600
			}
			tw, _ := c.MeasureText(0, ui.Span{Text: r.label, Size: 12, Weight: wgt})
			widths[i] = 2*regionChipPadX + tw
			if r.flag != "-" {
				widths[i] += regionFlag + regionFlagGap
			}
		}
		bw, _ := c.MeasureText(0, ui.Span{Text: label, Size: 12, Weight: 500})
		avail := w - 2*regionBarPad - 3*regionBarGap - regionSearchW - (bw + 2*11 + 2)
		strip := ui.ScrollHorizontal(c.Key("regions")).Gap(regionChipGap).Padding(12, 0).AlignItems(ui.Center).Shrink(1).MinWidth(0)
		if sw, cut := peekWidth(widths, regionChipGap, avail); cut {
			strip.Width(sw)
		}
		strip.Children(func() {
			for _, r := range regions {
				chip(r)
			}
		})
		ui.Spacer(c)
		ui.SearchField(c.Key("q"), &h.query).Placeholder(i18n.T("搜索节点", "Search nodes")).Width(regionSearchW).Shrink(0)
		b := btn(c, p, label).Disabled(testing).Shrink(0)
		b.OnClick(func() { u.do(func() error { return u.app.testRoute(bg(), rid) }) })
	})
	if r.Auto {
		ui.Text(c, i18n.T("自动：选延迟最低的可用节点，连续失败会自动换下一个（◆ 为当前选中）。点任一节点即切换为手动。", "Auto: picks the available node with the lowest delay and moves to the next after repeated failures (◆ is the current pick). Click any node to switch to manual.")).FontSize(12).TextColor(p.mute).Padding(10, 26, 0, 26)
	}
	var list []NodeView
	q := strings.ToLower(strings.TrimSpace(h.query))
	for _, n := range u.pickable(r) {
		if h.region == "?" && n.Where.Place != nil || h.region != "" && h.region != "?" && (n.Where.Place == nil || n.Where.Place.Country != h.region) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(n.Display), q) {
			continue
		}
		list = append(list, n)
	}
	sort.SliceStable(list, func(i, j int) bool { return rankOf(list[i]) < rankOf(list[j]) })
	if len(list) == 0 {
		ui.Text(c, i18n.T("没有符合条件的节点", "No nodes match")).FontSize(12.5).TextColor(p.mute).Padding(30, 26)
		return
	}
	ui.List(c.Key("nodes"), &h.nodes, len(list), func(i int) {
		n := list[i]
		on := u.picked(r, n)
		sw := u.pending("node:"+rid) && on
		row := ui.ButtonBase(c.Key(n.Name)).Padding(8, 26).Gap(10).Row().AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.hair2)
		ink, sub := p.ink, p.mute
		// The current node is marked quietly: a faint wash and a thin bar at
		// the left edge, not an inverted row that outshines the whole sheet.
		switch {
		case on && row.Hovered():
			row.Background(p.ink.Alpha(.09))
		case on:
			row.Background(p.ink.Alpha(.06))
		case row.Hovered():
			row.Background(p.hover)
		}
		row.OnClick(func() {
			if !on || r.Auto {
				u.do(func() error { return u.app.selectNode(rid, n.Name) })
			}
		}).Children(func() {
			if on {
				ui.Box(c).Absolute().Left(0).Top(0).Bottom(0).Width(2).Background(p.ink)
			}
			ui.Box(c).Width(22).Shrink(0).Children(func() { flagBadge(c, p, nodeFlag(n), 12) })
			// The name and its line give way to the delay: a long name ends
			// in "…" (its tooltip has it whole) instead of pushing the delay
			// past the sheet's edge.
			ui.Column(c).Grow(1).Shrink(1).Basis(0).MinWidth(0).Gap(2).Children(func() {
				shimmer(ui.Text(c, n.Display).FontSize(13).FontWeight(600).TextColor(ink).MaxLines(1).Ellipsis("…").Tooltip(n.Display), sw, p)
				ui.Row(c).Gap(6).AlignItems(ui.Center).MinWidth(0).Children(func() {
					ui.Text(c, placeName(n)).FontSize(11).TextColor(sub).NoWrap().Shrink(0)
					locTag(c, p, n.Where.Source)
					ui.Text(c, "· "+n.Type+" · "+u.providerName(n.Provider)).FontSize(11).TextColor(sub).MaxLines(1).Ellipsis("…").Shrink(1).MinWidth(0)
				})
			})
			if testing {
				ui.Box(c).Size(80, 12).Radius(p.r(3)).Background(sub.Alpha(.2)).Shrink(0)
			} else {
				stabilityBarsIn(c, p, ink.Alpha(.7), n.History, 3, 14).Shrink(0)
			}
			ui.Text(c, fmtDelay(n)).FontSize(12).FontFeatures("tnum").Width(58).Shrink(0).NoWrap().TextAlign(ui.End).TextColor(delayColor(p, n.Delay, n.Tested))
			mark := ""
			if on {
				mark = "✓"
				if r.Auto {
					mark = "◆"
				}
			}
			ui.Text(c, mark).FontSize(12).Width(14).Shrink(0).TextColor(ink)
		})
	}).Grow(1)
}

func rankOf(n NodeView) int {
	switch {
	case !n.Tested:
		return 1 << 20
	case n.Delay <= 0:
		return 1 << 21
	}
	return n.Delay
}

// ---- Mode ----

// modeWord is a mode's name.
func modeWord(m string) string {
	switch m {
	case "rule":
		return i18n.T("规则", "Rule")
	case "global":
		return i18n.T("全局", "Global")
	case "direct":
		return i18n.T("直连", "Direct")
	}
	return m
}

// modeNote is what a mode does, in a few words.
func modeNote(m string) string {
	switch m {
	case "rule":
		return i18n.T("按规则分到 5 条路由", "Split into 5 routes by rule")
	case "global":
		return i18n.T("所有流量走当前出口", "All traffic takes the current exit")
	case "direct":
		return i18n.T("不经过任何节点", "Through no node")
	}
	return ""
}

func (u *UI) modeNormal(c *ui.Context, p *pal) {
	s := u.snap
	col := pad(c, 16)
	// The three words stay on one line: wider than the cell (as with
	// "直接接続"), they get smaller; wrapping is the last resort.
	size, room := float32(25), col.LayoutBounds().W-52
	if room > 0 {
		for ; size > 17; size-- {
			w := float32(18 * 2)
			for _, m := range []string{"rule", "global", "direct"} {
				mw, _ := c.MeasureText(0, ui.Span{Text: modeWord(m), Size: size, Weight: 600})
				w += mw
			}
			if w <= room {
				break
			}
		}
	}
	col.Children(func() {
		cellKicker(c, p, "mode", i18n.T("模式", "Mode"))
		ui.Row(c).Wrap().GapX(18).GapY(4).Margin(10, 0, 0, 0).AlignItems(ui.Baseline).Children(func() {
			for _, m := range []string{"rule", "global", "direct"} {
				on := s.Mode == m
				b := ui.ButtonBase(c.Key(m)).OnClick(func() {
					if !on {
						u.do(func() error { return u.app.setMode(m) })
					}
				})
				b.Children(func() {
					col := p.faint
					if on {
						col = p.ink
					} else if b.Hovered() {
						col = p.ink2
					}
					shimmer(dispIn(ui.Text(c, modeWord(m)), p, col).FontSize(size).FontWeight(600).TextColor(col).
						Transition(ui.ElementTransition{Colors: true, Duration: 160 * time.Millisecond}), on && u.pending("mode"), p)
					if on {
						// The underline is one element that flies from word to word.
						ui.Box(c).Height(3).Background(p.ink).Margin(4, 0, 0, 0).Opacity(flyAlpha()).Transition(fly(modeMatch{}))
					} else {
						ui.Box(c).Height(3).Margin(4, 0, 0, 0)
					}
				})
			}
		})
		note := map[string]string{
			"rule":   fmt.Sprintf(i18n.T("按 %[1]d 条规则把流量分到 5 条路由；没命中的走默认。", "%d %s send traffic to 5 routes; what none matches takes Default."), s.RuleCount, rulesWord(s.RuleCount)),
			"global": i18n.T("所有流量都走当前出口，规则暂不生效。", "All traffic goes through the current exit; rules are paused."),
			"direct": i18n.T("所有流量直接连接，不经过任何节点。", "All traffic connects directly, through no node."),
		}[s.Mode]
		ui.Text(c, note).FontSize(12).TextColor(p.mute).Margin(12, 0, 0, 0).MaxLines(2).Ellipsis("…").Tooltip(note)
	})
}

func (u *UI) modeExpanded(c *ui.Context, p *pal) {
	s := u.snap
	u.detailHead(c, p, "mode", i18n.T("模式", "Mode"), func() { detailTitle(c, p, i18n.T("模式与规则", "Mode and rules")) }, nil)
	ui.Scroll(c).Grow(1).Padding(4, 26, 18, 26).Children(func() {
		for _, m := range [][3]string{
			{"rule", i18n.T("规则", "Rule"), i18n.T("按规则把流量分到不同路由。日常就用这个。", "Sends traffic to routes by rule. Use this day to day.")},
			{"global", i18n.T("全局", "Global"), i18n.T("所有流量都走「默认」路由的出口，规则暂停。临时需要时用。", "All traffic goes through the Default route's exit and rules are paused. For when you need it briefly.")},
			{"direct", i18n.T("直连", "Direct"), i18n.T("所有流量直接连接，不经过任何节点。排查网络问题时用。", "All traffic connects directly, through no node. For troubleshooting the network.")},
		} {
			id := m[0]
			section(c, p, m[1], map[bool]string{true: i18n.T("当前", "Current")}[s.Mode == id], func() {
				ui.Text(c, m[2]).FontSize(12.5).TextColor(p.mute)
			}, func() {
				label := i18n.T("切换", "Switch")
				if s.Mode == id {
					label = i18n.T("✓ 使用中", "✓ In use")
				}
				btn(c, p, label).OnClick(func() {
					if s.Mode != id {
						u.do(func() error { return u.app.setMode(id) })
					}
				})
			})
		}
		section(c, p, i18n.T("规则", "Rule"), fmt.Sprintf(i18n.T("%d 条 · bowel 默认模板", "%d · bowel's default template"), s.RuleCount), func() {
			ui.Column(c).Gap(6).Children(func() {
				for _, r := range s.Routes {
					ui.Row(c).Gap(10).Children(func() {
						ui.Row(c).Gap(6).Width(110).Children(func() {
							swatch(c, p.routeColor(r.ID), 8)
							ui.Text(c, r.Name).FontSize(12).TextColor(p.mute)
						})
						ui.Text(c, fmt.Sprintf(i18n.T("%d 条 · %s", "%d · %s"), r.Rules, r.Summary)).FontSize(12).TextColor(p.ink).Grow(1)
					})
				}
			})
		}, nil)
	})
}

// section is a row of an expanded view: a title on the left, the
// content, and a control on the right.
func section(c *ui.Context, p *pal, title, sub string, content func(), control func()) {
	ui.Row(c).Gap(18).Padding(14, 0).BorderWidth(0, 0, 1, 0).BorderColor(p.hair2).AlignItems(ui.Start).Children(func() {
		ui.Column(c).Width(180).Gap(3).Children(func() {
			disp(ui.Text(c, title), p).FontSize(18).FontWeight(700).TextColor(p.ink)
			if sub != "" {
				ui.Text(c, sub).FontSize(11.5).TextColor(p.mute)
			}
		})
		ui.Column(c).Grow(1).Children(content)
		if control != nil {
			control()
		}
	})
}

// ---- Takeover ----

func (u *UI) entryNames() []string {
	var names []string
	if t := u.snap.Takeover; t.On {
		if t.System.Wanted {
			names = append(names, i18n.T("系统代理", "System proxy"))
		}
		if t.Tun.Wanted {
			names = append(names, "TUN")
		}
		if t.Gateway.Wanted {
			names = append(names, i18n.T("网关", "Gateway"))
		}
	}
	return names
}

func (u *UI) takeoverNormal(c *ui.Context, p *pal) {
	pad(c, 16).Children(func() {
		cellKicker(c, p, "takeover", i18n.T("接管", "Takeover"))
		ui.Column(c).Margin(6, 0, 0, 0).Children(func() {
			u.entryWord(c, p, "sys", i18n.T("系统代理", "System proxy"), u.snap.Takeover.System)
			ui.Box(c).Height(1).BorderStyle(ui.BorderDashed).BorderWidth(1, 0, 0, 0).BorderColor(p.hair)
			u.entryWord(c, p, "tun", "TUN", u.snap.Takeover.Tun)
			if u.snap.Takeover.Gateway.Wanted {
				ui.Box(c).Height(1).BorderStyle(ui.BorderDashed).BorderWidth(1, 0, 0, 0).BorderColor(p.hair)
				u.gatewayWord(c, p)
			}
		})
	})
}

// gatewayWord is the gateway's line on the board, under the entries. Not
// a switch: a click opens the takeover, where its guide is.
func (u *UI) gatewayWord(c *ui.Context, p *pal) {
	t := u.snap.Takeover
	ui.Row(c).Wrap().Justify(ui.Start).Padding(6, 0).GapX(12).GapY(2).AlignItems(ui.Baseline).Children(func() {
		col := p.faint
		if t.On {
			col = p.ink
		}
		dispIn(ui.Text(c, i18n.T("网关", "Gateway")), p, col).FontSize(20).FontWeight(600).TextColor(col).NoWrap().Shrink(0).Grow(1)
		dot, text := u.gatewayStatus(p)
		ui.Row(c).Gap(6).Shrink(1).MinWidth(0).AlignItems(ui.Center).Children(func() {
			ui.Box(c).Size(6, 6).Radius(p.r(3)).Background(dot).Shrink(0)
			tcol := p.mute
			if dot == p.signal {
				tcol = p.signal
			}
			ui.Text(c, text).FontSize(11.5).TextColor(tcol).Shrink(1).MinWidth(0).MaxLines(1).Ellipsis("…")
		})
	})
}

func (u *UI) entryWord(c *ui.Context, p *pal, id, word string, e Entry) {
	t := u.snap.Takeover
	on := t.On && e.Wanted && e.Status == "on"
	busy := u.pending("entry:" + id)
	ui.ButtonBase(c.Key(id)).Row().Wrap().Justify(ui.Start).Padding(6, 0).GapX(12).GapY(2).AlignItems(ui.Baseline).OnClick(func() {
		want := !(t.On && e.Wanted)
		if id == "sys" {
			u.do(func() error { return u.app.setSystemProxy(want) })
		} else {
			u.do(func() error { return u.app.setTun(want) })
		}
	}).Children(func() {
		col := p.faint
		if t.On && e.Wanted {
			col = p.ink
		}
		// The word keeps its single line. When the status doesn't fit beside
		// it (long English labels in a narrow window) the row wraps and the
		// status drops under the word instead of splitting it.
		shimmer(dispIn(ui.Text(c, word), p, col).FontSize(20).FontWeight(600).TextColor(col).NoWrap().Shrink(0).Grow(1).Opacity(flyAlpha()).Transition(fly(entryMatch{id})), busy, p)
		dot, text, tcol := p.faint, i18n.T("未使用", "Not in use"), p.mute
		switch {
		case busy:
			text = i18n.T("正在应用…", "Applying…")
		case e.Status == "hijacked":
			dot, text, tcol = p.signal, i18n.T("被其他程序改掉了", "Changed by another app"), p.signal
		case e.Status == "error":
			dot, text, tcol = p.signal, e.Detail, p.signal
		case on && id == "sys":
			dot, text = p.good, i18n.T("生效中 · 浏览器和大多数应用", "Active · Browsers and most apps")
		case on:
			dot, text = p.good, i18n.T("生效中 · 所有应用", "Active · All apps")
		case id == "tun" && e.Status == "install":
			text = i18n.T("未使用 · 首次需要授权", "Not in use · Needs approval once")
		}
		ui.Row(c).Gap(6).Shrink(1).MinWidth(0).AlignItems(ui.Center).Children(func() {
			ui.Box(c).Size(6, 6).Radius(p.r(3)).Background(dot).Shrink(0)
			shimmer(ui.Text(c, text).FontSize(11.5).TextColor(tcol).Shrink(1).MinWidth(0), busy, p)
		})
	})
}

func (u *UI) takeoverExpanded(c *ui.Context, p *pal) {
	t := u.snap.Takeover
	u.detailHead(c, p, "takeover", i18n.T("接管", "Takeover"), func() { detailTitle(c, p, i18n.T("接管方式", "Takeover methods")) }, func() {
		on := t.On
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, i18n.T("接管", "Takeover")).FontSize(12.5).FontWeight(600).TextColor(p.ink)
			if ui.Switch(c.Key("master"), &on).Changed() {
				u.do(func() error { return u.app.setTakeover(on) })
			}
		})
	})
	ui.Scroll(c).Grow(1).Padding(4, 26, 18, 26).Children(func() {
		section(c, p, i18n.T("系统代理", "System proxy"), i18n.T("浏览器和大多数应用", "Browsers and most apps"), func() {
			status := i18n.T("未使用", "Not in use")
			switch {
			case u.pending("entry:sys"):
				status = i18n.T("正在应用…", "Applying…")
			case t.System.Status == "on":
				status = i18n.T("生效中", "Active")
			case t.System.Status == "hijacked":
				status = i18n.T("被其他程序改掉了 · ", "Changed by another app · ") + t.System.Detail
			}
			kv(c, p, [][2]string{{i18n.T("状态", "Status"), status}, {i18n.T("地址", "Address"), fmt.Sprintf("HTTP / HTTPS / SOCKS → 127.0.0.1:%d", t.Port)},
				{i18n.T("自检", "Self-check"), i18n.T("每 2 秒确认系统代理仍指向 bowel", "Checks every 2 seconds that the system proxy still points at bowel")}, {i18n.T("不走代理", "Bypass"), strings.Join(t.Bypass, ", ")}})
		}, func() {
			on := t.On && t.System.Wanted
			if ui.Switch(c.Key("sys"), &on).Disabled(u.pending("entry:sys")).Changed() {
				u.do(func() error { return u.app.setSystemProxy(on) })
			}
		})
		section(c, p, "TUN", i18n.T("所有应用，包括不认系统代理的", "All apps, including those that ignore the system proxy"), func() {
			status := i18n.T("未使用", "Not in use")
			switch {
			case u.pending("entry:tun") && t.Tun.Status == "install":
				status = i18n.T("正在安装，请在系统弹窗中确认…", "Installing; confirm in the system dialog…")
			case u.pending("entry:tun"):
				status = i18n.T("正在应用…", "Applying…")
			case t.Tun.Status == "on":
				status = i18n.T("生效中", "Active")
			case t.Tun.Status == "install":
				status = i18n.T("未使用 · 第一次开启时，macOS 会请求管理员授权", "Not in use · The first time it's turned on, macOS asks for an administrator's authorization")
			case t.Tun.Status == "error":
				status = t.Tun.Detail
			}
			kv(c, p, [][2]string{{i18n.T("状态", "Status"), status}, {i18n.T("协议栈", "Stack"), i18n.T("mixed（gVisor + system）", "mixed (gVisor + system)")}, {"DNS", i18n.T("劫持 53 端口 · fake-ip", "Hijacks port 53 · fake-ip")}})
		}, func() {
			on := t.On && t.Tun.Wanted
			if ui.Switch(c.Key("tun"), &on).Disabled(u.pending("entry:tun")).Changed() {
				u.do(func() error { return u.app.setTun(on) })
			}
		})
		section(c, p, i18n.T("本地端口", "Local port"), i18n.T("给手动配置的应用用", "For apps configured by hand"), func() {
			lan := i18n.T("不允许", "Not allowed")
			if t.Lan {
				lan = i18n.T("允许 · ", "Allowed · ") + t.LanAddr
			}
			kv(c, p, [][2]string{{i18n.T("混合端口", "Mixed port"), fmt.Sprintf(i18n.T("%d · HTTP 和 SOCKS5", "%d · HTTP and SOCKS5"), t.Port)}, {i18n.T("局域网", "LAN"), lan}})
		}, func() {
			on := t.Lan
			if ui.Switch(c.Key("lan"), &on).Changed() {
				u.do(func() error { return u.app.setLan(on) })
			}
		})
		section(c, p, i18n.T("网关", "Gateway"), i18n.T("局域网里的其他设备，把这台 Mac 当网关", "Other devices on the LAN, through this Mac as their gateway"), func() {
			_, status := u.gatewayStatus(p)
			addr := i18n.T("没有局域网地址", "No LAN address")
			if g := t.Gateway; g.IP != "" {
				link := i18n.T("有线", "Wired")
				if g.WiFi {
					link = "Wi‑Fi"
				}
				addr = fmt.Sprintf("%s · %s %s", g.IP, g.Iface, link)
			}
			rows := [][2]string{{i18n.T("状态", "Status"), status}}
			if t.Gateway.Wanted {
				rows = append(rows, [2]string{i18n.T("本机地址", "This Mac"), addr})
			} else {
				rows = append(rows, [2]string{i18n.T("作用", "What it does"), gatewayDesc()})
			}
			kv(c, p, rows)
			if u.gatewayShownOn() {
				ui.Column(c).Margin(12, 0, 0, 0).Children(func() { u.gatewayGuide(c, p, u.toast) })
			}
		}, func() { u.gatewaySwitch(c, "gateway", u.toast) })
	})
}

func kv(c *ui.Context, p *pal, rows [][2]string) {
	ui.Column(c).Gap(5).Children(func() {
		for _, r := range rows {
			ui.Row(c).Gap(12).Children(func() {
				ui.Text(c, r[0]).FontSize(12).TextColor(p.mute).Width(110)
				ui.Text(c, r[1]).FontSize(12).TextColor(p.ink).Grow(1)
			})
		}
	})
}

// ---- Routes ----

func (u *UI) routesNormal(c *ui.Context, p *pal) {
	pad(c, 16).Children(func() {
		cellKicker(c, p, "routes", i18n.T("其他路由", "Other routes"))
		var others []RouteView
		for _, r := range u.snap.Routes {
			if r.ID != "default" {
				others = append(others, r)
			}
		}
		// What the cell holds is five rows; a core with more shows the first
		// few and counts the rest.
		const rows = 5
		shown := others
		if len(others) > rows {
			shown = others[:rows-2]
		}
		ui.Column(c).Margin(4, 0, 0, 0).Children(func() {
			for _, r := range shown {
				u.routeRow(c, p, r)
			}
			if more := len(others) - len(shown); more > 0 {
				ui.ButtonBase(c.Key("more")).Padding(6, 0).OnClick(func() {
					if u.snap.Takeover.On {
						u.home.x, u.home.sub, u.home.query, u.home.region = "routes", "", "", ""
					}
				}).Children(func() {
					ui.Text(c, fmt.Sprintf(i18n.T("还有 %d 条路由", "%d more routes"), more)).FontSize(12).TextColor(p.mute)
				})
			}
		})
	})
}

func (u *UI) routeRow(c *ui.Context, p *pal, r RouteView) {
	rid := r.ID
	b := ui.ButtonBase(c.Key(rid)).Row().Gap(10).Padding(3, 0).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.hair2).
		OnClick(func() {
			if u.snap.Takeover.On {
				u.home.x, u.home.query, u.home.region = "routes", "", ""
				if !r.Direct {
					u.home.sub = rid
				}
			}
		})
	if !u.active(rid) {
		b.Opacity(.35)
	}
	b.Children(func() {
		swatch(c, p.routeColor(rid), 8)
		u.share(c, routeNameMatch{rid}, "route:"+rid, func(c *ui.Context) ui.Element {
			return ui.Text(c, r.Name).FontSize(12.5).FontWeight(600).TextColor(p.ink).Width(58).Shrink(0).NoWrap()
		})
		name, delay := i18n.T("直接连接", "Direct"), i18n.T("国内 · 局域网", "Domestic · LAN")
		n, hasNode := u.node(r.Node)
		hasNode = hasNode && !r.Direct
		if hasNode {
			name, delay = n.Display, fmtDelay(n)
		}
		u.share(c, exitMatch{rid}, "exit:"+rid, func(c *ui.Context) ui.Element {
			text := func() ui.Element {
				return shimmer(ui.Text(c, name).FontSize(14).FontWeight(600).TextColor(p.ink).Grow(1).Shrink(1).MinWidth(0).SingleLine().
					ContentTransition(morphText), u.pending("node:"+rid), p)
			}
			if hasNode {
				return flagged(c, p, n, 14, func() { text() }).Grow(1).Shrink(1).Basis(0).MinWidth(0)
			}
			return text()
		})
		ui.Text(c, delay).FontSize(11.5).FontFeatures("tnum").TextColor(p.mute).NoWrap().Shrink(0)
	})
}

func (u *UI) routesExpanded(c *ui.Context, p *pal, w float32) {
	h := &u.home
	if h.sub != "" {
		r := u.route(h.sub)
		u.detailHead(c, p, "routes", i18n.T("路由 · 选择出口", "Route · Choose exit"), func() {
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				iconBtn(c.Key("back"), p, "‹", 22, i18n.T("返回路由", "Back to routes")).Margin(0, -6, 0, -10).OnClick(func() { h.sub = "" })
				swatch(c, p.routeColor(r.ID), 10)
				u.share(c, routeNameMatch{r.ID}, "route:"+r.ID, func(c *ui.Context) ui.Element {
					return disp(ui.Text(c, r.Name), p).FontSize(22).FontWeight(700).TextColor(p.ink).NoWrap().Shrink(0)
				})
				ui.Text(c, "→").FontSize(18).TextColor(p.faint)
				n, ok := u.node(r.Node)
				u.share(c, exitMatch{r.ID}, "exit:"+r.ID, func(c *ui.Context) ui.Element {
					if !ok {
						return detailTitle(c, p, "—")
					}
					return flagged(c, p, n, 22, func() {
						// Two lines at most: a long name must leave the list its room.
						shimmer(detailTitle(c, p, n.Display).MaxLines(2).Ellipsis("…").Tooltip(n.Display).ContentTransition(morphText), u.pending("node:"+r.ID), p)
					})
				})
			})
		}, func() { u.autoSeg(c, p, r.ID) })
		u.nodePicker(c, p, r.ID, w)
		return
	}
	u.detailHead(c, p, "routes", i18n.T("路由", "Route"), func() { detailTitle(c, p, i18n.T("每类流量走哪里", "Where each kind of traffic goes")) }, func() {
		if !u.snap.External {
			btn(c.Key("rules"), p, i18n.T("管理规则", "Manage rules")).OnClick(u.openRules)
		}
	})
	ui.Scroll(c).Grow(1).Padding(4, 16, 18, 16).Children(func() {
		for _, r := range u.snap.Routes {
			rid := r.ID
			b := ui.ButtonBase(c.Key(rid)).Row().Gap(16).Padding(12, 10).AlignItems(ui.Center).Radius(p.r(8)).BorderWidth(0, 0, 1, 0).BorderColor(p.hair2)
			if !r.Direct {
				b.OnClick(func() { h.sub, h.query, h.region = rid, "", "" })
				if b.Hovered() {
					b.Background(p.hover)
				}
			}
			b.Children(func() {
				// The route and what it holds, on two lines: the summary
				// gives way first when the window is narrow.
				ui.Column(c).Gap(4).Grow(1).Shrink(1).Basis(0).MinWidth(100).Children(func() {
					ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
						swatch(c, p.routeColor(rid), 10).Shrink(0)
						u.share(c, routeNameMatch{rid}, "route:"+rid, func(c *ui.Context) ui.Element {
							return ui.Text(c, r.Name).FontSize(16).FontWeight(700).TextColor(p.ink).NoWrap()
						})
					})
					summary := r.Summary
					if r.Mine > 0 {
						summary = fmt.Sprintf(i18n.T("含你加的 %d 条 · %s", "%d of them yours · %s"), r.Mine, summary)
					}
					text := fmt.Sprintf(i18n.T("%[1]d 条规则 · %[3]s", "%d %s · %s"), r.Rules, rulesWord(r.Rules), summary)
					if summary == "" {
						text = fmt.Sprintf(i18n.T("%[1]d 条规则", "%d %s"), r.Rules, rulesWord(r.Rules))
					}
					ui.Text(c, text).
						FontSize(12).TextColor(p.mute).SingleLine().Shrink(1).Margin(0, 0, 0, 22)
				})
				name := i18n.T("直接连接", "Direct")
				n, hasNode := u.node(r.Node)
				hasNode = hasNode && !r.Direct
				if hasNode {
					name = n.Display
					if r.Auto {
						name += i18n.T(" · 自动", " · Auto")
					}
				}
				exit := func(c *ui.Context) ui.Element {
					text := func() ui.Element {
						return ui.Text(c, name).FontSize(15).FontWeight(600).TextColor(p.ink).SingleLine().Shrink(1).MinWidth(0).ContentTransition(morphText)
					}
					// A column of its own, so the flags line up; it gives way
					// to the route's name when the window is narrow.
					if hasNode {
						return flagged(c, p, n, 15, func() { text() }).Basis(220).Shrink(1).MinWidth(110)
					}
					return text().Basis(220).Shrink(1).MinWidth(110)
				}
				if rid != "default" {
					u.share(c, exitMatch{rid}, "exit:"+rid, exit)
				} else {
					exit(c)
				}
				d := "—"
				if hasNode {
					d = fmtDelay(n)
				}
				ui.Text(c, d).FontSize(12).FontFeatures("tnum").Width(56).Shrink(0).TextAlign(ui.End).TextColor(p.mute)
				ui.Text(c, fmt.Sprintf("%d%%", int(r.Share*100+.5))).FontSize(12).FontFeatures("tnum").Width(36).Shrink(0).TextAlign(ui.End).TextColor(p.mute)
				arrow := ""
				if !r.Direct {
					arrow = "›"
				}
				ui.Text(c, arrow).FontSize(14).TextColor(p.faint).Width(10).Shrink(0).TextAlign(ui.End)
			})
		}
	})
}

// ---- Speed ----

func (u *UI) speedNormal(c *ui.Context, p *pal) {
	t := u.tick
	pad(c, 14).Children(func() {
		cellKicker(c, p, "speed", i18n.T("此刻速度", "Speed now"))
		u.share(c, speedMatch{}, "speed", func(c *ui.Context) ui.Element {
			return speedLine(c, p, t)
		}).Margin(8, 0, 0, 0)
		u.trafficBars(c, p, 60).Height(44).Margin(10, 0, 0, 0)
		ui.Row(c).Margin(6, 0, 0, 0).Children(func() {
			ui.Text(c, fmt.Sprintf(i18n.T("今日 %s · 本月 %s", "Today %s · This month %s"), fmtBytes(t.TodayDown), fmtBytes(t.MonthDown))).FontSize(11).TextColor(p.mute).Grow(1)
			ui.Text(c, i18n.T("颜色 = 路由", "Color = route")).FontSize(11).TextColor(p.mute)
		})
	})
}

// speedLine is the speed cell's rates: down, then up.
func speedLine(c *ui.Context, p *pal, t Tick) ui.Element {
	return ui.Row(c).Gap(20).AlignItems(ui.Baseline).Children(func() {
		dv, du := splitRate(t.Down)
		uv, uu := splitRate(t.Up)
		ui.Row(c).AlignItems(ui.Baseline).Gap(3).Children(func() {
			disp(ui.Text(c, "↓ "+dv), p).FontSize(23).FontWeight(600).FontFeatures("tnum").TextColor(p.ink)
			ui.Text(c, du).FontSize(11.5).TextColor(p.mute)
		})
		ui.Row(c).AlignItems(ui.Baseline).Gap(3).Children(func() {
			ui.Text(c, "↑ "+uv).FontSize(15).FontWeight(600).FontFeatures("tnum").TextColor(p.ink2)
			ui.Text(c, uu).FontSize(11.5).TextColor(p.mute)
		})
	})
}

// trafficBars stacks each second's download by route, in route colors.
func (u *UI) trafficBars(c *ui.Context, p *pal, n int) ui.Element {
	hist := u.home.hist
	if len(hist) > n {
		hist = hist[len(hist)-n:]
	}
	order := []string{"direct", "dev", "ai", "default", "media"}
	return ui.Box(c).FillWidth().Draw(func(pt *ui.Painter, r ui.Rect) {
		if !u.snap.Takeover.On {
			pt.Fill(ui.Rect{X: r.X, Y: r.Y + r.H - 1, W: r.W, H: 1}, p.hair, 0)
			return
		}
		var peak int64 = 1
		for _, t := range hist {
			peak = max(peak, t.Down)
		}
		bw := r.W / float32(n)
		off := float32(n - len(hist))
		for i, t := range hist {
			y := r.Y + r.H
			for _, id := range order {
				v := t.ByRoute[id]
				if v <= 0 {
					continue
				}
				bh := r.H * float32(v) / float32(peak) / 1.05
				pt.Fill(ui.Rect{X: r.X + (off+float32(i))*bw + .5, Y: y - bh, W: max(bw-1.2, .5), H: bh}, p.routeColor(id).Alpha(.85), 0)
				y -= bh
			}
		}
	})
}

func (u *UI) speedExpanded(c *ui.Context, p *pal) {
	t := u.tick
	u.detailHead(c, p, "speed", i18n.T("流量", "Traffic"), func() {
		u.share(c, speedMatch{}, "speed", func(c *ui.Context) ui.Element {
			return ui.Row(c).Gap(16).AlignItems(ui.Baseline).Children(func() {
				detailTitle(c, p, "↓ "+fmtRate(t.Down))
				ui.Text(c, "↑ "+fmtRate(t.Up)).FontSize(14).TextColor(p.ink2).FontFeatures("tnum")
			})
		})
	}, nil)
	ui.Scroll(c).Grow(1).Padding(12, 26, 18, 26).Children(func() {
		u.trafficBars(c, p, 90).Height(170)
		ui.Text(c, i18n.T("最近 90 秒 · 按路由着色", "Last 90 seconds · Colored by route")).FontSize(11).TextColor(p.mute).Margin(6, 0, 10, 0)
		for _, r := range u.snap.Routes {
			ui.Row(c).Gap(14).Padding(10, 0).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.hair2).Children(func() {
				swatch(c, p.routeColor(r.ID), 10)
				ui.Text(c, r.Name).FontSize(14).FontWeight(700).TextColor(p.ink).Width(90)
				ui.Box(c).Grow(1).Height(4).Radius(p.r(2)).Background(p.hair).Children(func() {
					ui.Box(c).WidthPercent(float32(r.Share * 100)).FillHeight().Radius(p.r(2)).Background(p.routeColor(r.ID))
				})
				ui.Text(c, "↓ "+fmtRate(t.ByRoute[r.ID])).FontSize(12).FontFeatures("tnum").TextColor(p.ink).Width(100)
				ui.Text(c, fmt.Sprintf(i18n.T("今日 %d%%", "Today %d%%"), int(r.Share*100+.5))).FontSize(12).TextColor(p.mute).Width(70)
			})
		}
		ui.Text(c, fmt.Sprintf(i18n.T("今日 ↓ %s · ↑ %s　本月 ↓ %s", "Today ↓ %s · ↑ %s　This month ↓ %s"), fmtBytes(t.TodayDown), fmtBytes(t.TodayUp), fmtBytes(t.MonthDown))).FontSize(12).TextColor(p.mute).Margin(12, 0, 0, 0)
	})
}

// ---- Apps ----

func (u *UI) appsNormal(c *ui.Context, p *pal) {
	apps := u.tick.Apps
	var peak int64 = 1
	for _, a := range apps {
		peak = max(peak, a.Down)
	}
	pad(c, 14).Children(func() {
		cellKicker(c, p, "apps", i18n.T("谁在用", "Who's using it"))
		ui.Column(c).Gap(4).Margin(8, 0, 0, 0).Grow(1).Children(func() {
			if len(apps) == 0 {
				ui.Text(c, i18n.T("现在没有经过 bowel 的连接", "No connections through bowel right now")).FontSize(12).TextColor(p.mute)
			}
			for i, a := range apps {
				if i >= 4 {
					break
				}
				ui.Row(c.Key(a.App)).Gap(10).AlignItems(ui.Center).Transition(ui.ElementTransition{Animation: &motion}).Children(func() {
					ui.Row(c).Gap(6).Grow(1).Shrink(1).MinWidth(0).Children(func() {
						u.share(c, appMatch{a.App}, "app:"+a.App, func(c *ui.Context) ui.Element {
							// At most most of the room, so the host still shows.
							return ui.Text(c, a.App).FontSize(12).TextColor(p.ink).MaxLines(1).Ellipsis("…").Shrink(0).MaxWidth(110)
						})
						ui.Text(c, a.Host).FontSize(12).TextColor(p.mute).MaxLines(1).Ellipsis("…").Shrink(1).MinWidth(0)
					})
					ui.Box(c).Width(64).Height(3).Radius(p.r(2)).Background(p.hair).Shrink(0).Children(func() {
						ui.Box(c).WidthPercent(float32(a.Down) / float32(peak) * 100).FillHeight().Radius(p.r(2)).Background(p.routeColor(a.Route))
					})
					ui.Text(c, fmtRate(a.Down)).FontSize(12).FontFeatures("tnum").TextColor(p.ink2).Width(70).Shrink(0).NoWrap().TextAlign(ui.End)
				})
			}
		})
		ui.Row(c).Margin(6, 0, 0, 0).Children(func() {
			ui.Text(c, fmt.Sprintf(i18n.T("%d 个连接", "%d connections"), u.tick.Active)).FontSize(11).TextColor(p.mute).Grow(1)
			ui.Text(c, i18n.T("点开查看全部", "Open to see all")).FontSize(11).TextColor(p.mute)
		})
	})
}

func (u *UI) appsExpanded(c *ui.Context, p *pal) {
	apps := u.tick.Apps
	var peak int64 = 1
	for _, a := range apps {
		peak = max(peak, a.Down)
	}
	u.detailHead(c, p, "apps", i18n.T("谁在用", "Who's using it"), func() { detailTitle(c, p, i18n.T("应用", "Apps")) }, func() {
		btn(c, p, i18n.T("在活动中查看 →", "View in Log →")).OnClick(func() { u.page = "activity" })
	})
	ui.Scroll(c).Grow(1).Padding(4, 26, 18, 26).Children(func() {
		for _, a := range apps {
			app := a.App
			b := ui.ButtonBase(c.Key(app)).Row().Gap(14).Padding(11, 6).AlignItems(ui.Center).Radius(p.r(6)).BorderWidth(0, 0, 1, 0).BorderColor(p.hair2).
				OnClick(func() { u.act.filterApp(app); u.page = "activity" })
			if b.Hovered() {
				b.Background(p.hover)
			}
			b.Children(func() {
				u.share(c, appMatch{app}, "app:"+app, func(c *ui.Context) ui.Element {
					return ui.Text(c, app).FontSize(15).FontWeight(700).TextColor(p.ink).Width(160).Shrink(0).MaxLines(1).Ellipsis("…").Tooltip(app)
				})
				ui.Text(c, a.Host).FontSize(12).TextColor(p.mute).Grow(1).Shrink(1).Basis(0).MinWidth(0).MaxLines(1).Ellipsis("…")
				ui.Box(c).Width(140).Height(3).Radius(p.r(2)).Background(p.hair).Shrink(0).Children(func() {
					ui.Box(c).WidthPercent(float32(a.Down) / float32(peak) * 100).FillHeight().Radius(p.r(2)).Background(p.routeColor(a.Route))
				})
				ui.Text(c, fmtRate(a.Down)).FontSize(12).FontFeatures("tnum").TextColor(p.ink).Width(90).Shrink(0).NoWrap().TextAlign(ui.End)
				ui.Text(c, fmt.Sprintf(i18n.T("%d 连接", "%d conns"), a.Conns)).FontSize(12).TextColor(p.mute).MinWidth(60).Shrink(0).NoWrap()
				ui.Text(c, "›").FontSize(13).TextColor(p.mute).Shrink(0)
			})
		}
		if len(apps) == 0 {
			ui.Text(c, i18n.T("现在没有经过 bowel 的连接", "No connections through bowel right now")).FontSize(12.5).TextColor(p.mute).Padding(20, 0)
		}
	})
}

// ---- Events ----

func (u *UI) eventLine(c *ui.Context, p *pal, ev Event, i int) {
	ui.Row(c.Key(ev.Time.UnixNano() + int64(i))).Gap(8).Transition(ui.ElementTransition{Animation: &motion, Enter: &ui.Motion{Y: -4}}).Children(func() {
		ui.Text(c, ev.Time.Format("04:05")).FontSize(11.5).FontFeatures("tnum").TextColor(p.faint).Width(40)
		col := p.ink2
		if ev.Failed {
			col = p.signal
		}
		where := u.route(ev.Route).Name
		if ev.Node != "" {
			where += " · " + ev.Node
		}
		if ev.Failed {
			where += i18n.T(" · 失败", " · Failed")
		}
		ui.RichText(c, ui.Span{Text: ev.Text, Color: col, Size: 11.5}, ui.Span{Text: "  " + where, Color: p.mute, Size: 11.5}).MaxLines(1).Ellipsis("…").Grow(1).Shrink(1).MinWidth(0)
	})
}

func (u *UI) eventsNormal(c *ui.Context, p *pal) {
	pad(c, 14).Children(func() {
		cellKicker(c, p, "events", i18n.T("刚刚", "Just now"))
		ui.Column(c).Gap(5).Margin(8, 0, 0, 0).ClipY().Children(func() {
			if !u.snap.Takeover.On || len(u.tick.Events) == 0 {
				ui.Text(c, i18n.T("没有经过 bowel 的流量", "No traffic through bowel")).FontSize(11.5).TextColor(p.mute)
			}
			for i, ev := range u.tick.Events {
				if i >= 6 {
					break
				}
				u.eventLine(c, p, ev, i)
			}
		})
	})
}

func (u *UI) eventsExpanded(c *ui.Context, p *pal) {
	u.detailHead(c, p, "events", i18n.T("刚刚", "Just now"), func() { detailTitle(c, p, i18n.T("实时事件", "Live events")) }, func() {
		btn(c, p, i18n.T("完整日志 →", "Full log →")).OnClick(func() { u.act.showLogs(); u.page = "activity" })
	})
	ui.Scroll(c).Grow(1).Padding(8, 26, 18, 26).Gap(7).Children(func() {
		for i, ev := range u.tick.Events {
			u.eventLine(c, p, ev, i)
		}
	})
}

// rulesWord is "rule" or "rules" for n, in English; Chinese has no plural
// and its format strings leave the word out.
func rulesWord(n int) string {
	if n == 1 {
		return "rule"
	}
	return "rules"
}

// pickable is what a route can be pointed at: every node for the template's
// routes, and for an external core's the members of the route's own group.
func (u *UI) pickable(r RouteView) []NodeView {
	if u.snap.External {
		return r.Members
	}
	return u.snap.Nodes
}

// picked is whether n is what the route is pointed at.
func (u *UI) picked(r RouteView, n NodeView) bool {
	if u.snap.External {
		return n.Name == r.Picked
	}
	return n.Name == r.Node
}

// coreCell stands for the mode and takeover cells when the core is
// someone else's: bowel sets neither, so it says which core this is and
// where it listens.
func (u *UI) coreCell(c *ui.Context, p *pal, id, v string) {
	s := u.snap
	label, value, note := i18n.T("内核", "Core"), s.Core.Version, i18n.T("外部内核，由它自己管理", "External core, managed by itself")
	if id == "takeover" {
		label, value, note = i18n.T("控制器", "Controller"), s.ControllerTCP, i18n.T("bowel 只切换节点，不改动它", "bowel only switches nodes, nothing else")
	}
	if v != "normal" {
		compact(c, p, v, id, label, func(size float32) {
			disp(ui.Text(c, value), p).FontSize(size).FontWeight(600).TextColor(p.ink).NoWrap()
		}, note, nil)
		return
	}
	pad(c, 16).Children(func() {
		cellKicker(c, p, id, label)
		disp(ui.Text(c, value), p).FontSize(20).FontWeight(600).TextColor(p.ink).MaxLines(2).Ellipsis("…").Margin(10, 0, 0, 0)
		ui.Text(c, note).FontSize(12).TextColor(p.mute).Margin(12, 0, 0, 0).MaxLines(2).Ellipsis("…")
	})
}
