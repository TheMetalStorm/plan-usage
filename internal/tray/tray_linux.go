//go:build linux && cgo

package tray

/*
#cgo pkg-config: gtk+-3.0
#include <stdint.h>
#include <gtk/gtk.h>
#include <gdk/gdk.h>
static void plan_usage_grab_add(uintptr_t widget) {
	GtkWidget *gtk_widget = GTK_WIDGET((void *)widget);
	GdkWindow *window = gtk_widget_get_window(gtk_widget);
	if (window == NULL) return;
	GdkDisplay *display = gdk_window_get_display(window);
	GdkSeat *seat = gdk_display_get_default_seat(display);
	if (seat == NULL) return;
	(void)gdk_seat_grab(seat, window, GDK_SEAT_CAPABILITY_ALL, TRUE, NULL, NULL, NULL, NULL);
}
static void plan_usage_grab_remove(uintptr_t widget) {
	GtkWidget *gtk_widget = GTK_WIDGET((void *)widget);
	GdkWindow *window = gtk_widget_get_window(gtk_widget);
	if (window == NULL) return;
	GdkSeat *seat = gdk_display_get_default_seat(gdk_window_get_display(window));
	if (seat != NULL) gdk_seat_ungrab(seat);
}
*/
import "C"

import (
	"context"
	"encoding/base64"
	"fmt"
	"runtime"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/TheMetalStorm/plan-usage/internal/config"
	"github.com/TheMetalStorm/plan-usage/internal/daemon"
	"github.com/TheMetalStorm/plan-usage/internal/state"
	"github.com/TheMetalStorm/plan-usage/internal/types"
	"github.com/gotk3/gotk3/gdk"
	"github.com/gotk3/gotk3/glib"
	"github.com/gotk3/gotk3/gtk"
)

const usageIconBase64 = "iVBORw0KGgoAAAANSUhEUgAAABAAAAAQCAYAAAAf8/9hAAAALUlEQVR42mP4TyFgABESCgZkYbIMMLpTAcZDyACYBnQ8SA3ApmjUgCFjACUAADUiaw93Ogv/AAAAAElFTkSuQmCC"

// Run starts the real Linux/X11 tray popup. GTK_WINDOW_POPUP is an
// override-redirect X11 window, so i3 does not add it to its managed tree.
func Run(cfg *config.Config) error {
	if err := checkDesktopSession(); err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := gtk.InitCheck(nil); err != nil {
		return fmt.Errorf("initialize GTK: %w; Linux/X11 with DISPLAY is required", err)
	}
	display, err := gdk.DisplayGetDefault()
	if err != nil || display == nil {
		return fmt.Errorf("initialize GTK display: %w; Linux/X11 with DISPLAY is required", err)
	}
	name, err := display.GetName()
	if err != nil || name == "" || len(name) >= 7 && name[:7] == "wayland" {
		return fmt.Errorf("Wayland is not supported: GTK selected %q; run plan-usage tray in an X11 session", name)
	}

	poller, err := daemon.New(cfg)
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	store, err := state.New(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	popup, err := newPopup(cfg, poller, store, display)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var endTray func()
	quit := make(chan struct{})
	var quitOnce sync.Once
	requestQuit := func() {
		quitOnce.Do(func() {
			close(quit)
			_ = popup.runOnGTK(func() {
				popup.hide()
				gtk.MainQuit()
			})
		})
	}

	onReady := func() {
		systray.SetIcon(mustIcon())
		systray.SetTooltip("plan-usage: coding-plan usage")
		systray.SetTitle(windowTitle)
		systray.SetOnTapped(func() { _ = popup.runOnGTK(popup.toggle) })
		systray.SetOnSecondaryTapped(func() {})
		refreshItem := systray.AddMenuItem("Refresh now", "Refresh all enabled providers")
		quitItem := systray.AddMenuItem("Quit", "Exit plan-usage")
		go func() {
			for {
				select {
				case <-refreshItem.ClickedCh:
					popup.requestRefresh()
				case <-quitItem.ClickedCh:
					requestQuit()
				case <-quit:
					return
				}
			}
		}()
	}
	onExit := func() { requestQuit() }
	start, end := systray.RunWithExternalLoop(onReady, onExit)
	endTray = end
	start()

	go popup.refreshLoop(ctx)
	popup.requestRefresh()
	gtk.Main()
	popup.markStopped()
	cancel()
	if endTray != nil {
		endTray()
	}
	return nil
}

func mustIcon() []byte {
	icon, err := base64.StdEncoding.DecodeString(usageIconBase64)
	if err != nil {
		return nil
	}
	return icon
}

type popup struct {
	cfg        *config.Config
	poller     *daemon.Daemon
	store      *state.Store
	display    *gdk.Display
	window     *gtk.Window
	scroll     *gtk.ScrolledWindow
	cards      *gtk.Grid
	status     *gtk.Label
	gate       RefreshGate
	rendered   []*gtk.Box
	emptyLabel *gtk.Label
	stopMu     sync.RWMutex
	stopped    bool
}

func newPopup(cfg *config.Config, poller *daemon.Daemon, store *state.Store, display *gdk.Display) (*popup, error) {
	window, err := gtk.WindowNew(gtk.WINDOW_POPUP)
	if err != nil {
		return nil, fmt.Errorf("create GTK popup: %w", err)
	}
	window.SetTitle(windowTitle)
	window.SetDefaultSize(popupWidth, popupHeight)
	window.SetBorderWidth(popupBorder)
	window.SetCanFocus(true)
	window.SetEvents(int(gdk.KEY_PRESS_MASK | gdk.BUTTON_PRESS_MASK | gdk.STRUCTURE_MASK))

	root, err := gtk.BoxNew(gtk.ORIENTATION_VERTICAL, popupSpacing)
	if err != nil {
		return nil, fmt.Errorf("create popup layout: %w", err)
	}
	header, err := gtk.BoxNew(gtk.ORIENTATION_HORIZONTAL, popupSpacing)
	if err != nil {
		return nil, err
	}
	header.SetName("header")
	title, err := gtk.LabelNew("plan-usage")
	if err != nil {
		return nil, err
	}
	title.SetHAlign(gtk.ALIGN_START)
	title.SetName("popup-title")
	title.SetMarkup("<b>plan-usage</b> <span alpha='75%'>usage overview</span>")
	refresh, err := gtk.ButtonNewWithLabel("Refresh")
	if err != nil {
		return nil, err
	}
	refresh.SetName("refresh-button")
	closeButton, err := gtk.ButtonNewWithLabel("Hide")
	if err != nil {
		return nil, err
	}
	closeButton.SetName("hide-button")
	header.PackStart(title, true, true, 0)
	header.PackEnd(closeButton, false, false, 0)
	header.PackEnd(refresh, false, false, 0)

	cards, err := gtk.GridNew()
	if err != nil {
		return nil, err
	}
	cards.SetColumnSpacing(popupSpacing)
	cards.SetRowSpacing(popupSpacing)
	cards.SetColumnHomogeneous(true)
	scroll, err := gtk.ScrolledWindowNew(nil, nil)
	if err != nil {
		return nil, err
	}
	scroll.SetPolicy(gtk.POLICY_NEVER, gtk.POLICY_AUTOMATIC)
	scroll.Add(cards)
	status, err := gtk.LabelNew("Waiting for first refresh…")
	if err != nil {
		return nil, err
	}
	status.SetHAlign(gtk.ALIGN_START)
	status.SetLineWrap(true)
	status.SetName("statusbar")
	root.PackStart(header, false, false, 0)
	root.PackStart(scroll, true, true, 0)
	root.PackStart(status, false, false, 0)
	window.Add(root)

	p := &popup{cfg: cfg, poller: poller, store: store, display: display, window: window, scroll: scroll, cards: cards, status: status}
	refresh.Connect("clicked", func(_ *gtk.Button) { p.requestRefresh() })
	closeButton.Connect("clicked", func(_ *gtk.Button) { p.hide() })
	window.Connect("key-press-event", func(_ *gtk.Window, event *gdk.Event) bool {
		if gdk.EventKeyNewFromEvent(event).KeyVal() == gdk.KEY_Escape {
			p.hide()
			return true
		}
		return false
	})
	window.Connect("focus-out-event", func(_ *gtk.Window, _ *gdk.Event) bool { p.hide(); return false })
	window.Connect("button-press-event", func(_ *gtk.Window, event *gdk.Event) bool {
		button := gdk.EventButtonNewFromEvent(event)
		width, height := p.window.GetSize()
		if button.Button() == 1 && PopupClickOutside(button.X(), button.Y(), width, height) {
			p.hide()
			return true
		}
		return false
	})
	if err := installCSS(); err != nil {
		return nil, err
	}
	return p, nil
}

func installCSS() error {
	provider, err := gtk.CssProviderNew()
	if err != nil {
		return err
	}
	if err := provider.LoadFromData(`
		window { background: #0d1117; color: #e6edf3; }
		scrolledwindow, scrolledwindow viewport { background: #0d1117; border: none; }
		#header { background: #161b22; border: 1px solid #30363d; border-radius: 10px; padding: 8px 10px; }
		#popup-title { color: #f0f6fc; font-size: 15px; }
		button { background: #21262d; color: #c9d1d9; border: 1px solid #30363d; border-radius: 6px; padding: 5px 10px; }
		button:hover { background: #30363d; color: #f0f6fc; border-color: #58a6ff; }
		button:active { background: #1f6feb; color: #ffffff; }
		#refresh-button { background: #238636; color: #ffffff; border-color: #2ea043; font-weight: bold; }
		#refresh-button:hover { background: #2ea043; border-color: #3fb950; }
		#hide-button:hover { border-color: #8b949e; }
		#card { background: #161b22; border: 1px solid #30363d; border-radius: 8px; padding: 8px; }
		#card:hover { background: #1c2128; border-color: #58a6ff; }
		#card-error { background: #21151a; border: 1px solid #f85149; border-radius: 8px; padding: 8px; }
		#card-title { color: #f0f6fc; font-size: 13px; }
		#card-meta { color: #8b949e; font-size: 10px; }
		#card-status { color: #8b949e; font-size: 11px; }
		#usage-heading { color: #58a6ff; font-size: 10px; font-weight: bold; }
		#usage-line { color: #e6edf3; font-size: 11px; font-weight: bold; }
		#usage-meta { color: #8b949e; font-size: 10px; }
		#usage-progress trough { background: #21262d; min-height: 7px; border-radius: 4px; }
		#usage-progress progress { background: #2ea043; min-height: 7px; border-radius: 4px; }
		#free-model-heading { color: #d2a8ff; font-size: 11px; font-weight: bold; }
		#free-model { color: #c9d1d9; font-size: 11px; }
		#updated { color: #6e7681; font-size: 9px; }
		#statusbar { color: #8b949e; background: #161b22; border: 1px solid #21262d; border-radius: 6px; padding: 5px 8px; font-size: 10px; }
		#empty-state { color: #8b949e; font-size: 12px; padding: 16px; }
	`); err != nil {
		return err
	}
	screen, err := gdk.ScreenGetDefault()
	if err != nil {
		return err
	}
	gtk.AddProviderForScreen(screen, provider, gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
	return nil
}

func (p *popup) runOnGTK(fn func()) error {
	p.stopMu.RLock()
	stopped := p.stopped
	p.stopMu.RUnlock()
	if stopped {
		return fmt.Errorf("GTK main loop is stopped")
	}
	glib.IdleAdd(func() bool {
		p.stopMu.RLock()
		stopped := p.stopped
		p.stopMu.RUnlock()
		if !stopped {
			fn()
		}
		return false
	})
	return nil
}

func (p *popup) markStopped() {
	p.stopMu.Lock()
	p.stopped = true
	p.stopMu.Unlock()
}

func (p *popup) toggle() {
	if p.window.GetVisible() {
		p.hide()
		return
	}
	p.showAtPointer()
}

func (p *popup) hide() {
	C.plan_usage_grab_remove(C.uintptr_t(p.window.Native()))
	p.window.Hide()
}

func (p *popup) showAtPointer() {
	seat, err := p.display.GetDefaultSeat()
	if err != nil {
		p.window.ShowAll()
		return
	}
	pointer, err := seat.GetPointer()
	if err != nil {
		p.window.ShowAll()
		return
	}
	var screen *gdk.Screen
	x, y := 0, 0
	if err := pointer.GetPosition(&screen, &x, &y); err != nil {
		p.window.ShowAll()
		return
	}
	p.window.ShowAll()
	width, height := p.window.GetSize()
	monitor, err := p.display.GetMonitorAtPoint(x, y)
	if err != nil || monitor == nil {
		p.window.Move(x, y)
		return
	}
	work := monitor.GetWorkarea()
	workRect := Rect{X: work.GetX(), Y: work.GetY(), Width: work.GetWidth(), Height: work.GetHeight()}
	width, height = popupSizeForWorkarea(width, height, workRect)
	p.window.Resize(width, height)
	px, py := PopupPosition(x, y, width, height, workRect)
	p.window.Move(px, py)
	p.window.GrabFocus()
	C.plan_usage_grab_add(C.uintptr_t(p.window.Native()))
}

func (p *popup) requestRefresh() {
	if !p.gate.TryBegin() {
		return
	}
	go func() {
		defer p.gate.End()
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		p.poller.Refresh(ctx)
		if err := p.store.Load(); err != nil {
			_ = p.runOnGTK(func() { p.status.SetText("Refresh completed, but snapshot could not be read: " + err.Error()) })
			return
		}
		agg := p.store.All()
		_ = p.runOnGTK(func() { p.render(agg) })
	}()
}

func (p *popup) refreshLoop(ctx context.Context) {
	for {
		timer := time.NewTimer(p.cfg.RefreshEvery)
		select {
		case <-timer.C:
			p.requestRefresh()
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
}

func (p *popup) render(agg types.Aggregate) {
	for _, child := range p.rendered {
		child.Destroy()
	}
	if p.emptyLabel != nil {
		p.emptyLabel.Destroy()
		p.emptyLabel = nil
	}
	p.rendered = nil
	cards := BuildCards(p.cfg, agg)
	if len(cards) == 0 {
		p.emptyLabel, _ = gtk.LabelNew("No providers are enabled.")
		p.emptyLabel.SetHAlign(gtk.ALIGN_START)
		p.emptyLabel.SetName("empty-state")
		p.cards.Attach(p.emptyLabel, 0, 0, popupGridColumns, 1)
	} else {
		for i, card := range cards {
			cardWidget := renderCard(card)
			cardWidget.SetHExpand(true)
			cardWidget.SetVExpand(false)
			p.rendered = append(p.rendered, cardWidget)
			column, row := popupGridPosition(i)
			p.cards.Attach(cardWidget, column, row, 1, 1)
		}
	}
	p.cards.ShowAll()
	p.status.SetText(fmt.Sprintf("%d provider(s) · refreshed %s", len(cards), aggregateAge(agg.GeneratedAt)))
}

func renderCard(card ProviderCard) *gtk.Box {
	box, _ := gtk.BoxNew(gtk.ORIENTATION_VERTICAL, popupCardSpacing)
	if card.Error != "" {
		box.SetName("card-error")
	} else {
		box.SetName("card")
	}
	title, _ := gtk.LabelNew("")
	title.SetHAlign(gtk.ALIGN_START)
	title.SetName("card-title")
	title.SetMarkup(fmt.Sprintf("<b>%s  %s</b>", escapeMarkup(card.Icon), escapeMarkup(card.DisplayName)))
	box.PackStart(title, false, false, 0)

	status, _ := gtk.LabelNew("")
	status.SetHAlign(gtk.ALIGN_START)
	status.SetLineWrap(true)
	status.SetName("card-status")
	status.SetMarkup(escapeMarkup(card.Status))
	box.PackStart(status, false, false, 0)

	usageBox, _ := gtk.BoxNew(gtk.ORIENTATION_VERTICAL, popupCardSpacing)
	freeModels := freeModelsOnly(card.Models)
	premiumModels := []types.FreeModel(nil)
	if card.Name == "freebuff" {
		premiumModels = premiumModelsOnly(card.Models)
	}
	box.PackStart(usageBox, false, false, 0)

	if len(card.Windows) > 0 {
		usageHeading, _ := gtk.LabelNew("Usage")
		usageHeading.SetHAlign(gtk.ALIGN_START)
		usageHeading.SetName("usage-heading")
		usageBox.PackStart(usageHeading, false, false, 0)
	}
	for _, window := range card.Windows {
		label, _ := gtk.LabelNew(fmt.Sprintf("%s  %.0f%%  %s / %s", window.Label, window.Percent, window.Used, window.Total))
		label.SetHAlign(gtk.ALIGN_START)
		label.SetLineWrap(true)
		label.SetName("usage-line")
		usageBox.PackStart(label, false, false, 0)
		bar, _ := gtk.ProgressBarNew()
		bar.SetFraction(window.Percent / 100)
		bar.SetName("usage-progress")
		bar.SetShowText(true)
		bar.SetText(fmt.Sprintf("%.0f%%", window.Percent))
		usageBox.PackStart(bar, false, false, 0)
		meta, _ := gtk.LabelNew(fmt.Sprintf("%s%s", window.Reset, noteSuffix(window.Note)))
		meta.SetHAlign(gtk.ALIGN_START)
		meta.SetLineWrap(true)
		meta.SetName("usage-meta")
		usageBox.PackStart(meta, false, false, 0)
	}
	if len(card.Windows) == 0 {
		noUsage, _ := gtk.LabelNew("No usage window")
		noUsage.SetHAlign(gtk.ALIGN_START)
		noUsage.SetName("usage-meta")
		usageBox.PackStart(noUsage, false, false, 0)
	}

	if len(freeModels) > 0 || len(premiumModels) > 0 {
		modelsBox, _ := gtk.BoxNew(gtk.ORIENTATION_VERTICAL, popupCardSpacing)
		if len(freeModels) > 0 {
			appendModelSection(modelsBox, "Free models", freeModels)
		}
		if len(premiumModels) > 0 {
			appendModelSection(modelsBox, "Premium models (free for 6h/day)", premiumModels)
		}
		box.PackStart(modelsBox, false, false, 0)
	}

	updated, _ := gtk.LabelNew(card.Updated)
	updated.SetHAlign(gtk.ALIGN_START)
	updated.SetName("updated")
	box.PackStart(updated, false, false, 0)
	return box
}

func appendModelSection(parent *gtk.Box, heading string, models []types.FreeModel) {
	label, _ := gtk.LabelNew(heading)
	label.SetHAlign(gtk.ALIGN_START)
	label.SetName("free-model-heading")
	parent.PackStart(label, false, false, 0)
	for _, model := range models {
		name := model.Label
		if name == "" {
			name = model.ID
		}
		if name == "" {
			continue
		}
		if model.Notes != "" {
			name += " · " + model.Notes
		}
		item, _ := gtk.LabelNew("• " + name)
		item.SetHAlign(gtk.ALIGN_START)
		item.SetLineWrap(true)
		item.SetMaxWidthChars(popupModelMaxChars)
		item.SetName("free-model")
		parent.PackStart(item, false, false, 0)
	}
}

func escapeMarkup(s string) string {
	return glib.MarkupEscapeText(s)
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return " · " + note
}
