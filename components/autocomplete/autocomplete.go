package autocomplete

import (
	"context"
	"log"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotkit/app"
	"github.com/diamondburned/gotkit/gtkutil/cssutil"
)

type ctxKey uint

const (
	_ ctxKey = iota
	iterDataCtx
)

// WhitespaceRune is a special rune that Searcher can return to indicate that it
// should be activated on every word.
const WhitespaceRune rune = ' '

// Searcher is the interface for anything that can handle searching up a
// particular entity, such as a room member.
type Searcher interface {
	// Rune is the triggering rune for this searcher. If ' ' is returned, then
	// the searcher is always invoked on a complete word.
	Rune() rune
	// Search searches the given string and returns a list of data. The returned
	// list of Data only needs to be valid until the next call of Search.
	Search(ctx context.Context, str string) []Data
}

// IterData contains iterator data that's given to Searcher.Search's context.
// Use IterDataFromContext to get it.
type IterData struct {
	Start *gtk.TextIter
	End   *gtk.TextIter
}

// IterDataFromContext returns the IterData from the given context. If the
// context does not contain any IterData, then it panics.
func IterDataFromContext(ctx context.Context) *IterData {
	data, _ := ctx.Value(iterDataCtx).(*IterData)
	if data == nil {
		panic("no iter data in context")
	}
	return data
}

// Data represents a data structure capable of being displayed inside a list by
// constructing a new ListBoxRow.
type Data interface {
	// Row constructs a new ListBoxRow for display inside the list.
	Row(context.Context) *gtk.ListBoxRow
}

// SelectedData wraps around a Data to provide additional metadata that could be
// useful for the user.
type SelectedData struct {
	// Bounds contains the iterators that sit around the word used for
	// searching. The iterators are guaranteed to be valid until the callback
	// returns.
	Bounds [2]*gtk.TextIter
	// Data is the selected entry's data.
	Data Data
}

// SelectedFunc is the callback type that is called when the user has selected
// an entry inside the autocompleter. If the callback returns true, then the
// autocompleter closes itself; otherwise, it does nothing.
type SelectedFunc func(SelectedData) bool

// Autocompleter is the autocompleter instance.
type Autocompleter struct {
	tview  *gtk.TextView
	buffer *gtk.TextBuffer

	start *gtk.TextIter
	end   *gtk.TextIter

	onSelects []SelectedFunc

	popover  *gtk.Popover
	listBox  *gtk.ListBox
	listRows []row

	searchers map[rune]Searcher

	parent         context.Context
	cancel         context.CancelFunc
	minChars       int
	timeout        time.Duration
	poppedUp       bool
	cancelOnChange bool
	paused         bool
}

type row struct {
	*gtk.ListBoxRow
	data Data
}

var _ = cssutil.WriteCSS(`
	.autocomplete-row {
		padding: 2px 6px;
	}
	.autocomplete-row label:nth-child(1) {
		margin-bottom: -2px;
	}
	.autocomplete-row label:nth-child(2) {
		margin-top: -2px;
	}
`)

// AutocompleterWidth is the minimum width of the popped up autocompleter.
const AutocompleterWidth = 250

// MaxResults is the maximum number of search results.
const MaxResults = 8

// New creates a new instance of autocompleter.
func New(ctx context.Context, text *gtk.TextView) *Autocompleter {
	list := gtk.NewListBox()
	list.AddCSSClass("autocomplete-list")
	list.SetActivateOnSingleClick(true)
	list.SetSelectionMode(gtk.SelectionBrowse)

	viewport := gtk.NewViewport(nil, nil)
	viewport.SetVScrollPolicy(gtk.ScrollNatural)
	viewport.SetScrollToFocus(true)
	viewport.SetChild(list)

	scroll := gtk.NewScrolledWindow()
	scroll.AddCSSClass("autocomplete-list-scroll")
	scroll.SetChild(viewport)
	scroll.SetMinContentHeight(0)
	scroll.SetMaxContentHeight(250)
	scroll.SetPropagateNaturalHeight(true)

	popover := gtk.NewPopover()
	popover.AddCSSClass("autocomplete-popover")
	popover.SetSizeRequest(AutocompleterWidth, -1)
	popover.SetParent(text)
	popover.SetChild(scroll)
	popover.SetPosition(gtk.PosTop)
	popover.SetAutohide(false)
	popover.Hide()

	ac := Autocompleter{
		parent:    ctx,
		tview:     text,
		buffer:    text.Buffer(),
		popover:   popover,
		listBox:   list,
		listRows:  make([]row, 0, MaxResults),
		searchers: make(map[rune]Searcher),
		onSelects: make([]SelectedFunc, 0, 1),
	}

	text.ConnectUnmap(func() {
		// Ensure the context is cleaned up.
		if ac.cancel != nil {
			ac.cancel()
		}
	})

	list.ConnectRowActivated(func(row *gtk.ListBoxRow) {
		ac.selectRow(row)
	})

	return &ac
}

// Pause pauses the autocompleter. The popover is hidden if it's currently being
// shown.
func (a *Autocompleter) Pause() {
	a.paused = true
	a.Clear()
}

// Unpause unpauses the autocompleter.
func (a *Autocompleter) Unpause() {
	a.paused = false
}

// SetPopoverWidth sets the width of the popover. The default width is 250px.
func (a *Autocompleter) SetPopoverWidth(width int) {
	a.popover.SetSizeRequest(width, -1)
}

// SetMinLength sets the minimum number of characters before the autocompleter
// kicks in.
func (a *Autocompleter) SetMinLength(minLength int) {
	a.minChars = minLength
}

// SetCancelOnChange sets whether or not the autocompleter context should be
// cancelled every time the buffer is changed. Default is false, so the context
// is alive even when the old widgets are thrown away.
func (a *Autocompleter) SetCancelOnChange(v bool) {
	a.cancelOnChange = v
}

// SetTimeout sets the timeout for each autocompletion.
func (a *Autocompleter) SetTimeout(d time.Duration) {
	a.timeout = d
}

// AddSelectedFunc adds a callback that is called when the user has selected an
// entry inside the autocompleter.
func (a *Autocompleter) AddSelectedFunc(selectedFunc SelectedFunc) {
	a.onSelects = append(a.onSelects, selectedFunc)
}

// Use registers the given searcher instance into the autocompleter.
func (a *Autocompleter) Use(searchers ...Searcher) {
	for _, s := range searchers {
		if _, ok := a.searchers[s.Rune()]; ok {
			log.Panicf("autocompleter: duplicate rune %q for searcher %T", s.Rune(), s)
		}
		a.searchers[s.Rune()] = s
	}
}

// Unuse removes the given searcher instance from the autocompleter using the
// given identifying rune.
func (a *Autocompleter) Unuse(searcher Searcher) {
	for r, s := range a.searchers {
		if s == searcher && r == searcher.Rune() {
			delete(a.searchers, r)
			return
		}
	}
}

// Autocomplete updates the Autocompleter popover to show what the internal
// input buffer has.
func (a *Autocompleter) Autocomplete() {
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}

	a.clear()

	if a.paused {
		a.hide()
		return
	}

	cursor := a.buffer.ObjectProperty("cursor-position").(int)

	a.start = a.buffer.IterAtOffset(cursor)
	a.end = a.buffer.IterAtOffset(cursor)

	var searcher Searcher

	if !a.start.BackwardFindChar(func(ch uint32) bool {
		r := rune(ch)
		if unicode.IsSpace(r) {
			// If we stumbled upon a space character, then we haven't found
			// anything yet inside a.searchers that resembles a non-whitespace
			// rune, so we just grab one here.
			searcher = a.searchers[WhitespaceRune]
			return true // stop scanning
		}

		var ok bool
		searcher, ok = a.searchers[r]
		return ok
	}, nil) || searcher == nil {
		// If we haven't managed to find anything and we're at the start of the
		// line, then we probably want to use the WhitespaceRune as well.
		if whitespaceSearcher, ok := a.searchers[WhitespaceRune]; ok {
			searcher = whitespaceSearcher
		} else {
			a.hide()
			return
		}
	}

	// Remove the prefix.
	a.start.ForwardChar()

	// Forward the cursor until either end of buffer or until the word
	// terminates.
	if a.end.ForwardFindChar(func(ch uint32) bool {
		return unicode.IsSpace(rune(ch))
	}, nil) {
		// Space found. Shift it away.
		a.end.BackwardChar()
	}

	text := a.buffer.Text(a.start, a.end, false)
	if utf8.RuneCountInString(text) < a.minChars {
		a.hide()
		return
	}

	// Include the prefix again.
	a.start.BackwardChar()

	// cancelled on next run
	ctx := a.parent
	if a.cancelOnChange {
		ctx, a.cancel = context.WithCancel(a.parent)
	}

	// Inject iter data.
	ctx = context.WithValue(ctx, iterDataCtx, &IterData{
		Start: a.start,
		End:   a.end,
	})

	searchCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	results := searcher.Search(searchCtx, text)
	if len(results) == 0 {
		a.hide()
		return
	}

	for _, result := range results {
		r := row{
			ListBoxRow: result.Row(ctx),
			data:       result,
		}

		r.AddCSSClass("autocomplete-row")

		a.listBox.Append(r.ListBoxRow)
		a.listRows = append(a.listRows, r)
	}

	a.listBox.SelectRow(a.listRows[0].ListBoxRow)
	a.show()
}

// IsVisible returns true if the popover is currently visible.
func (a *Autocompleter) IsVisible() bool {
	return len(a.listRows) > 0 && a.popover.IsVisible()
}

// Select selects the current Autocompleter entry.
func (a *Autocompleter) Select() bool {
	if len(a.listRows) == 0 || !a.IsVisible() {
		return false
	}
	a.selectRow(a.listBox.SelectedRow())
	return true
}

func (a *Autocompleter) selectRow(row *gtk.ListBoxRow) {
	if row == nil {
		a.Clear()
		return
	}

	data := SelectedData{
		Bounds: [2]*gtk.TextIter{a.start, a.end},
		Data:   a.listRows[row.Index()].data,
	}

	for _, onSelect := range a.onSelects {
		if onSelect(data) {
			a.buffer.Insert(data.Bounds[1], " ")
			a.Clear()
			return
		}
	}
}

// Clear clears the Autocompleter and hides it.
func (a *Autocompleter) Clear() bool {
	if !a.IsVisible() {
		return false
	}

	a.clear()
	a.hide()
	return true
}

func (a *Autocompleter) hide() {
	if a.poppedUp {
		a.popover.Popdown()
		a.poppedUp = false
	}
}

func (a *Autocompleter) show() {
	if !a.poppedUp {
		a.poppedUp = true

		// Put the popover at the start of the word so we can avoid shifting the
		// popover, otherwise it gets a bit annoying.
		rect := a.tview.IterLocation(a.start)
		x, y := a.tview.BufferToWindowCoords(gtk.TextWindowWidget, rect.X(), rect.Y())

		ptTo := gdk.NewRectangle(x, y, 1, 1)
		a.popover.SetPointingTo(&ptTo)
		a.popover.Popup()
	}
}

func (a *Autocompleter) clear() {
	for i, r := range a.listRows {
		a.listBox.Remove(r.ListBoxRow)
		a.listRows[i] = row{}
	}
	a.listRows = a.listRows[:0]
}

func (a *Autocompleter) MoveUp() bool   { return a.move(false) }
func (a *Autocompleter) MoveDown() bool { return a.move(true) }

func (a *Autocompleter) move(down bool) bool {
	if len(a.listRows) == 0 {
		return false
	}

	row := a.listBox.SelectedRow()
	if row == nil {
		a.listBox.SelectRow(a.listRows[0].ListBoxRow)
		return true
	}

	ix := row.Index()
	if down {
		ix++
		if ix == len(a.listRows) {
			ix = 0
		}
	} else {
		ix--
		if ix == -1 {
			ix = len(a.listRows) - 1
		}
	}

	a.listBox.SelectRow(a.listRows[ix].ListBoxRow)

	// Steal focus. This is a hack to scroll to the selected item without having
	// to manually calculate the coordinates.
	focused := gtk.BaseWidget(app.WindowFromContext(a.parent).Focus())
	a.listRows[ix].ListBoxRow.GrabFocus()
	focused.GrabFocus()

	return true
}
