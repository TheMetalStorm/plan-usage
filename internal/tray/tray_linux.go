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
	cards      *gtk.Box
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
	window.SetDefaultSize(440, 620)
	window.SetBorderWidth(10)
	window.SetCanFocus(true)
	window.SetEvents(int(gdk.KEY_PRESS_MASK | gdk.BUTTON_PRESS_MASK | gdk.STRUCTURE_MASK))

	root, err := gtk.BoxNew(gtk.ORIENTATION_VERTICAL, 8)
	if err != nil {
		return nil, fmt.Errorf("create popup layout: %w", err)
	}
	header, err := gtk.BoxNew(gtk.ORIENTATION_HORIZONTAL, 8)
	if err != nil {
		return nil, err
	}
	title, err := gtk.LabelNew("plan-usage")
	if err != nil {
		return nil, err
	}
	title.SetHAlign(gtk.ALIGN_START)
	title.SetMarkup("<b>plan-usage</b> <span alpha='75%'>usage overview</span>")
	refresh, err := gtk.ButtonNewWithLabel("Refresh")
	if err != nil {
		return nil, err
	}
	closeButton, err := gtk.ButtonNewWithLabel("Hide")
	if err != nil {
		return nil, err
	}
	header.PackStart(title, true, true, 0)
	header.PackEnd(closeButton, false, false, 0)
	header.PackEnd(refresh, false, false, 0)

	cards, err := gtk.BoxNew(gtk.ORIENTATION_VERTICAL, 8)
	if err != nil {
		return nil, err
	}
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
		window { background: #182235; color: #f4f7fb; }
		#card { background: #24324a; border: 1px solid #405475; border-radius: 8px; padding: 10px; }
		#card-error { background: #432b38; border: 1px solid #c45b70; border-radius: 8px; padding: 10px; }
		#card-meta { color: #b9c7da; font-size: 10px; }
		#card-status { color: #d8e2f0; font-size: 11px; }
		progressbar trough { background: #111827; min-height: 8px; border-radius: 4px; }
		progressbar progress { background: #39d98a; min-height: 8px; border-radius: 4px; }
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
	px, py := PopupPosition(x, y, width, height, Rect{X: work.GetX(), Y: work.GetY(), Width: work.GetWidth(), Height: work.GetHeight()})
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
		p.cards.PackStart(p.emptyLabel, false, false, 0)
	} else {
		for _, card := range cards {
			cardWidget := renderCard(card)
			p.rendered = append(p.rendered, cardWidget)
			p.cards.PackStart(cardWidget, false, false, 0)
		}
	}
	p.cards.ShowAll()
	p.status.SetText(fmt.Sprintf("%d provider(s) · refreshed %s", len(cards), aggregateAge(agg.GeneratedAt)))
}

func renderCard(card ProviderCard) *gtk.Box {
	box, _ := gtk.BoxNew(gtk.ORIENTATION_VERTICAL, 5)
	if card.Error != "" {
		box.SetName("card-error")
	} else {
		box.SetName("card")
	}
	title, _ := gtk.LabelNew("")
	title.SetHAlign(gtk.ALIGN_START)
	title.SetMarkup(fmt.Sprintf("<b>%s  %s</b>", escapeMarkup(card.Icon), escapeMarkup(card.DisplayName)))
	box.PackStart(title, false, false, 0)
	status, _ := gtk.LabelNew("")
	status.SetHAlign(gtk.ALIGN_START)
	status.SetLineWrap(true)
	status.SetName("card-status")
	status.SetMarkup(escapeMarkup(card.Status))
	box.PackStart(status, false, false, 0)
	for _, window := range card.Windows {
		label, _ := gtk.LabelNew(fmt.Sprintf("%s  %.0f%%  %s / %s", window.Label, window.Percent, window.Used, window.Total))
		label.SetHAlign(gtk.ALIGN_START)
		box.PackStart(label, false, false, 0)
		bar, _ := gtk.ProgressBarNew()
		bar.SetFraction(window.Percent / 100)
		bar.SetShowText(true)
		bar.SetText(fmt.Sprintf("%.0f%%", window.Percent))
		box.PackStart(bar, false, false, 0)
		meta, _ := gtk.LabelNew(fmt.Sprintf("%s%s", window.Reset, noteSuffix(window.Note)))
		meta.SetHAlign(gtk.ALIGN_START)
		meta.SetName("card-meta")
		box.PackStart(meta, false, false, 0)
	}
	updated, _ := gtk.LabelNew(card.Updated)
	updated.SetHAlign(gtk.ALIGN_START)
	updated.SetName("card-meta")
	box.PackStart(updated, false, false, 0)
	return box
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
