package embed

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotkit/app"
	"github.com/diamondburned/gotkit/gtkutil"
	"github.com/diamondburned/gotkit/gtkutil/imgutil"
)

// TODO: In libadwaita 1.4 replace BackButton with `set_show_back_button“
type Viewer struct {
	*adw.Window

	Header       *adw.HeaderBar
	ToastOverlay *adw.ToastOverlay
	Overlay      *gtk.Overlay
	Scroll       *gtk.ScrolledWindow
	Embed        *Embed

	BackButton    *gtk.Button
	ControlsStart ControlsBoxStart
	ControlsEnd   ControlsBoxEnd

	vadj  *gtk.Adjustment
	hadj  *gtk.Adjustment

	zoom float64
	filename string

	ctx context.Context
}

type ControlsBoxStart struct {
	*gtk.Box

	Download     *gtk.Button
	CopyURL      *gtk.Button
	OpenOriginal *gtk.Button
}

type ControlsBoxEnd struct {
	*gtk.Box
}

var ControlsStyles = []string{"osd", "circular"}

// NewViewer creates a new instance of Viewer window, representing an image viewer.
func NewViewer(ctx context.Context, uri string, opts Opts) (*Viewer, error) {
	v := Viewer{ctx: ctx}
	v.Embed = New(ctx, 0, 0, opts)
	v.Embed.SetFromURL(uri)

	v.ToastOverlay = adw.NewToastOverlay()

	v.Overlay = gtk.NewOverlay()

	v.ToastOverlay.SetChild(v.Overlay)

	v.Scroll = gtk.NewScrolledWindow()
	v.Scroll.SetVExpand(true)
	v.Scroll.SetHExpand(true)

	v.Overlay.SetChild(v.Scroll)

	v.zoom = 1.0

	v.vadj = v.Scroll.VAdjustment()
	v.hadj = v.Scroll.HAdjustment()

	v.Window = adw.NewWindow()
	v.SetSizeRequest(360, 360)
	v.SetTransientFor(app.GTKWindowFromContext(ctx))
	v.SetModal(true)

	url, err := url.Parse(v.Embed.URL())
	if err != nil {
		fmt.Printf("Invalid raw URI structure: %s\n", url)
		return nil, err
	}
	v.filename = path.Base(url.Path)

	v.BackButton = newActionButton(v, "Back", "go-previous-symbolic", "embedviewer.close", nil)

	v.Header = adw.NewHeaderBar()
	v.Header.SetShowEndTitleButtons(false)
	v.Header.SetShowStartTitleButtons(false)
	v.Header.SetCenteringPolicy(adw.CenteringPolicyStrict)
	v.Header.SetTitleWidget(adw.NewWindowTitle(v.filename, ""))

	v.SetShowBackButton(true)

	v.ControlsStart = ControlsBoxStart{
		Box:          gtk.NewBox(gtk.OrientationHorizontal, 6),
		Download:     newActionButton(v, "Download", "folder-download-symbolic", "embedviewer.download", ControlsStyles),
		CopyURL:      newActionButton(v, "Copy URL", "edit-copy-symbolic", "embedviewer.copy-url", ControlsStyles),
		OpenOriginal: newActionButton(v, "Open Original", "earth-symbolic", "embedviewer.open-original", ControlsStyles),
	}

	v.ControlsStart.SetMarginBottom(18)
	v.ControlsStart.SetMarginStart(18)
	v.ControlsStart.SetHAlign(gtk.AlignStart)
	v.ControlsStart.SetVAlign(gtk.AlignEnd)

	v.ControlsStart.Append(v.ControlsStart.OpenOriginal)
	v.ControlsStart.Append(v.ControlsStart.Download)
	v.ControlsStart.Append(v.ControlsStart.CopyURL)

	v.ControlsEnd = ControlsBoxEnd{
		Box: gtk.NewBox(gtk.OrientationHorizontal, 6),
	}

	v.ControlsEnd.SetMarginBottom(18)
	v.ControlsEnd.SetMarginStart(18)
	v.ControlsEnd.SetHAlign(gtk.AlignEnd)
	v.ControlsEnd.SetVAlign(gtk.AlignEnd)

	v.Overlay.AddOverlay(v.ControlsStart)
	v.Overlay.AddOverlay(v.ControlsEnd)

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.Append(v.Header)
	box.Append(v.ToastOverlay)

	v.SetContent(box)

	gtkutil.BindActionMap(v, map[string]func(){
		"embedviewer.close":         v.close,
		"embedviewer.download":      v.download,
		"embedviewer.copy-url":      v.copyURL,
		"embedviewer.open-original": v.openOriginal,
	})

	switch opts.Type {
	case EmbedTypeImage, EmbedTypeGIF:
		v.Embed.SetHExpand(true)
		v.Embed.SetVExpand(true)

		// Keep original size of the image when resizing window
		v.Embed.SetVAlign(gtk.AlignCenter)
		v.Embed.SetHAlign(gtk.AlignCenter)

		v.Scroll.SetChild(v.Embed)
		v.Scroll.SetPolicy(gtk.PolicyAutomatic, gtk.PolicyAutomatic)

		// Disable click-to-open so we can handle panning.
		v.Embed.SetOpenURL(nil)
		v.Embed.NotifyImage(func() {
			v.scaleFit()
		})

		// Keep track of the scroll begin coordinates so we can get the offset
		// properly.
		var originX, originY float64

		dragCtrl := gtk.NewGestureDrag()
		dragCtrl.ConnectDragBegin(func(x, y float64) {
			originX = v.hadj.Value()
			originY = v.vadj.Value()
		})
		dragCtrl.ConnectDragUpdate(func(offsetX, offsetY float64) {
			v.hadj.SetValue(originX - offsetX)
			v.vadj.SetValue(originY - offsetY)
		})

		v.Scroll.AddController(dragCtrl)

	case EmbedTypeGIFV, EmbedTypeVideo:
		v.Embed.SetVExpand(true)
		v.Embed.SetHExpand(true)
		v.Embed.SetVAlign(gtk.AlignFill)
		v.Embed.SetHAlign(gtk.AlignFill)

		v.Scroll.SetChild(v.Embed)
		v.Scroll.SetPolicy(gtk.PolicyNever, gtk.PolicyNever)
	default:
		err := fmt.Errorf("unsupported embed type: %#v", opts.Type)
		return nil, err
	}

	return &v, nil
}

func newActionButton(target gtk.Widgetter, text, icon, action string, styles []string) *gtk.Button {
	button := gtk.NewButtonFromIconName(icon)
	button.SetTooltipText(text)

	if styles != nil {
		button.SetCSSClasses(styles)
	}

	button.ConnectClicked(func() {
		base := gtk.BaseWidget(target)
		base.ActivateAction(action, nil)
	})

	return button
}

// SetShowBackButton sets whether to show back button at the start of headerbar.
func (v *Viewer) SetShowBackButton(show bool) {
	if !show {
		v.Header.Remove(v.BackButton)
	}

	v.Header.PackStart(v.BackButton)
}

// AddStartButton adds a button into the ControlsBoxStart.
func (cs *ControlsBoxStart) AddStartButton(pack gtk.PositionType, button *gtk.Button) {
	switch pack {
	case gtk.PosTop, gtk.PosLeft:
		cs.Prepend(button)
	case gtk.PosBottom, gtk.PosRight:
		cs.Append(button)
	}
}

// AddEndButton adds a button into the ControlsBoxEnd.
func (ce *ControlsBoxEnd) AddEndButton(pack gtk.PositionType, button *gtk.Button) {
	switch pack {
	case gtk.PosTop, gtk.PosLeft:
		ce.Prepend(button)
	case gtk.PosBottom, gtk.PosRight:
		ce.Append(button)
	}
}

func (v *Viewer) close() {
	v.Close()
}

func (v *Viewer) download() {
	chooser := gtk.NewFileChooserNative(
		"",
		app.GTKWindowFromContext(v.ctx),
		gtk.FileChooserActionSave,
		"Save", "Cancel",
	)
	chooser.SetModal(true)
	chooser.SetCurrentName(v.filename)

	chooser.ConnectResponse(func(resp int) {
		if resp == int(gtk.ResponseAccept) {
			file := chooser.File()
			v.saveToFile(file, v.Embed.URL())
		}
	})

	chooser.Show()
}

func (v *Viewer) saveToFile(file *gio.File, pictureURL string) {
	outPath := file.Path()

	response, err := http.Get(pictureURL)
	if err != nil {
		v.ToastOverlay.AddToast(adw.NewToast("An error occured while downloading picture data"))
		fmt.Println("An error occured while downloading picture data:", err)
		return
	}
	defer response.Body.Close()

	out, err := os.Create(outPath)
	if err != nil {
		v.ToastOverlay.AddToast(adw.NewToast("An I/O error occurred while creating the output file"))
		fmt.Println("An I/O error occurred while creating the output file:", err)
		return
	}
	defer out.Close()

	_, err = io.Copy(out, response.Body)
	if err != nil {
		v.ToastOverlay.AddToast(adw.NewToast("An I/O error occurred while saving the file"))
		fmt.Println("An I/O error occurred while saving the file:", err)
		return
	}

	v.ToastOverlay.AddToast(adw.NewToast("Picture saved successfully"))
}

func (v *Viewer) copyURL() {
	url := v.Embed.URL()

	display := gdk.DisplayGetDefault()

	clipboard := display.Clipboard()
	clipboard.SetText(url)

	v.ToastOverlay.AddToast(adw.NewToast("Copied URL!"))
}

func (v *Viewer) openOriginal() {
	app.OpenURI(v.ctx, v.Embed.URL())
}

func (v *Viewer) scaleFit() {
	viewportAlloc := v.Scroll.Allocation()

	vpw := viewportAlloc.Width()
	vph := viewportAlloc.Height()

	w, h := v.Embed.Size()

	newW, newH := imgutil.MaxSize(w, h, vpw, vph)
	v.Embed.SetSizeRequest(newW, newH)

	// Calculate the current scale. The aspect ratio might be diffrent, so we
	// get the smallest one.
	wscale := float64(vpw) / float64(w)
	hscale := float64(vph) / float64(h)
	v.zoom = math.Min(wscale, hscale)
}
