//go:build !(darwin && !cgo) && !(linux && arm)

package main

import (
	"context"
	"time"

	"fyne.io/systray"
	"github.com/ncruces/zenity"

	"github.com/patrick-1984/ai-minion/internal/tray"
)

const pollEvery = 5 * time.Second

// dialogs are the native dialogs of zenity (a Windows dialog, osascript on
// macOS, the zenity program on Linux).
type dialogs struct{}

func (dialogs) Token() (string, bool) {
	s, err := zenity.Entry("Paste the join token of your Hub:",
		zenity.Title("Minion: set up the Hub"), zenity.HideText(), zenity.OKLabel("Set up"))
	return s, err == nil
}

func (dialogs) Confirm(text string) bool {
	return zenity.Question(text, zenity.Title("Minion"), zenity.OKLabel("Yes"), zenity.CancelLabel("No")) == nil
}

func (dialogs) Pick(text string, all, checked []string) ([]string, bool) {
	out, err := zenity.ListMultiple(text, all, zenity.Title("Minion: shared sessions"),
		zenity.DefaultItems(checked...), zenity.OKLabel("Save"))
	return out, err == nil
}

func (dialogs) Info(text string)  { _ = zenity.Info(text, zenity.Title("Minion")) }
func (dialogs) Error(text string) { _ = zenity.Error(text, zenity.Title("Minion")) }

// menu is the tray menu; Show is called from any goroutine.
type menu struct {
	status, hub, keys         *systray.MenuItem
	start, stop, pause        *systray.MenuItem
	setup, share, newSessions *systray.MenuItem
}

func (m *menu) Show(s tray.Status, err error) {
	if err != nil {
		systray.SetIcon(iconBytes(colorRed))
		systray.SetTooltip("Minion: " + err.Error())
		m.status.SetTitle("minion: " + err.Error())
		return
	}
	switch {
	case s.Mismatch():
		systray.SetIcon(iconBytes(colorRed))
	case s.Running() && !s.Paused:
		systray.SetIcon(iconBytes(colorGreen))
	default:
		systray.SetIcon(iconBytes(colorGray))
	}
	systray.SetTooltip("Minion: " + s.StatusLine())
	m.status.SetTitle(s.StatusLine())
	m.hub.SetTitle(s.HubLine())
	m.keys.SetTitle(s.KeysLine())
	if s.Running() {
		m.start.Disable()
		m.stop.Enable()
	} else {
		m.start.Enable()
		m.stop.Disable()
	}
	if s.Paused {
		m.pause.SetTitle("Resume")
	} else {
		m.pause.SetTitle("Pause")
	}
	if s.NewSessions == "off" {
		m.newSessions.Uncheck()
	} else {
		m.newSessions.Check()
	}
}

func runTray() error {
	path, err := tray.FindMinion()
	if err != nil {
		return err
	}
	m := &menu{}
	c := &tray.Controller{R: tray.ExecRunner{Path: path}, D: dialogs{}, V: m}
	ctx, cancel := context.WithCancel(context.Background())
	systray.Run(func() {
		systray.SetIcon(iconBytes(colorGray))
		systray.SetTitle("Minion")
		m.status = systray.AddMenuItem("Minion", "")
		m.hub = systray.AddMenuItem("Hub: ?", "The Hub this Worker is paired with")
		m.keys = systray.AddMenuItem("Hub key: ?", "Compare these six words with the ones the Hub shows")
		m.status.Disable()
		m.hub.Disable()
		m.keys.Disable()
		systray.AddSeparator()
		m.start = systray.AddMenuItem("Start", "minion worker start")
		m.stop = systray.AddMenuItem("Stop", "minion worker down")
		m.pause = systray.AddMenuItem("Pause", "minion worker pause / resume")
		systray.AddSeparator()
		m.setup = systray.AddMenuItem("Set up the Hub...", "minion worker up --join -")
		m.share = systray.AddMenuItem("Shared sessions...", "minion worker config share")
		m.newSessions = systray.AddMenuItemCheckbox("Allow new sessions", "minion worker config set allow_new_sessions", true)
		systray.AddSeparator()
		refresh := systray.AddMenuItem("Refresh", "minion worker status --json")
		quit := systray.AddMenuItem("Quit tray", "Closes the tray only; the Worker keeps running")
		go c.Refresh()
		go c.Poll(ctx, pollEvery)
		go func() {
			for {
				select {
				case <-m.start.ClickedCh:
					go c.Start()
				case <-m.stop.ClickedCh:
					go c.Stop()
				case <-m.pause.ClickedCh:
					go c.TogglePause()
				case <-m.setup.ClickedCh:
					go c.Setup()
				case <-m.share.ClickedCh:
					go c.ShareSessions()
				case <-m.newSessions.ClickedCh:
					go c.ToggleNewSessions()
				case <-refresh.ClickedCh:
					go c.Refresh()
				case <-quit.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()
	}, cancel)
	return nil
}
