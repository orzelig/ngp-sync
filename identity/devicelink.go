package main

// Phone codes are confirmed before sync access (ADR 0006). A code minted by
// Add a phone (linkEntry.Phone) does not redeem to a token at POST
// /device/link: it redeems to a pending request, which the account owner
// confirms or cancels on the account page.
//
//   1. Phone        POST /device/link {"code","device"} → 202
//                   {"status":"pending","poll_id","expires_in":300,"interval":2}
//   2. Account page POST /account/device/confirm | /account/device/cancel
//                   (session + origin + CSRF, form field id = the pending ID)
//   3. Phone        POST /device/link/poll {"poll_id"} → pending, confirmed
//                   with the token fields (once), cancelled (403) or unknown (401)
//
// A browser code (/callback, /dev/enroll) is not affected: shipped ngp.31
// browsers do not poll, so they still get the token at once.
//
// The poll ID is a credential: 256 bits, returned once, never on a page or in a
// URL. The map key, and the ID the account page's forms carry, is its hash.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	// pendingLinkTTL is how long the owner has to confirm a phone. An entry is
	// kept as long again after that (expired reads as cancelled), then reaped.
	pendingLinkTTL = 5 * time.Minute
	// pendingLinkPollInterval is the poll interval, in seconds, the phone is told.
	pendingLinkPollInterval = 2
)

type pendingLinkState int

const (
	linkPending pendingLinkState = iota
	linkConfirmed
	linkCancelled
)

// pendingLink is one phone waiting for its owner, keyed in App.pendingLinks by
// hashToken(poll ID).
type pendingLink struct {
	XID       string
	Device    string
	Requested time.Time
	Expires   time.Time
	State     pendingLinkState
	Refresh   string // plaintext, only between Confirm and the poll that collects it
}

// pendingView is one pending request on the account page.
type pendingView struct {
	ID   string // the pending ID (hash of the poll ID), never the poll ID
	Name string
	Ago  string
}

func (a *App) registerDeviceLinkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /device/link/poll", a.handleDeviceLinkPoll)
	mux.HandleFunc("POST /account/device/confirm", a.handleDeviceConfirm)
	mux.HandleFunc("POST /account/device/cancel", a.handleDeviceCancel)
}

// startPendingLink answers POST /device/link for a spent phone code: it records
// a pending request and issues nothing.
func (a *App) startPendingLink(w http.ResponseWriter, xid, device string) {
	pollID := randHex(32)
	now := time.Now()
	a.mu.Lock()
	if a.pendingLinks == nil {
		a.pendingLinks = map[string]pendingLink{}
	}
	a.pendingLinks[hashToken(pollID)] = pendingLink{
		XID:       xid,
		Device:    strings.TrimSpace(device),
		Requested: now,
		Expires:   now.Add(pendingLinkTTL),
	}
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":     "pending",
		"poll_id":    pollID,
		"expires_in": int(pendingLinkTTL.Seconds()),
		"interval":   pendingLinkPollInterval,
	})
}

func (a *App) handleDeviceLinkPoll(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PollID string `json:"poll_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "bad request body")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	id := hashToken(in.PollID)
	now := time.Now()

	a.mu.Lock()
	p, ok := a.pendingLinks[id]
	switch {
	case !ok:
		a.mu.Unlock()
		jsonError(w, http.StatusUnauthorized, "unknown poll id")
	case p.State == linkConfirmed:
		// The device may have been removed since Confirm (Remove, Remove all):
		// then there is nothing to hand out.
		if _, live := a.store.LookupRefreshToken(p.Refresh); !live {
			p.State, p.Refresh = linkCancelled, ""
			a.pendingLinks[id] = p
			a.mu.Unlock()
			writeCancelled(w)
			return
		}
		out, err := a.tokenResponse(p.XID, p.Refresh, accessTokenTTL)
		if err != nil {
			a.mu.Unlock() // left confirmed: the next poll tries again
			jsonError(w, http.StatusInternalServerError, "token mint failed")
			return
		}
		delete(a.pendingLinks, id) // the token is given once
		a.mu.Unlock()
		out["status"] = "confirmed"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	case p.State == linkCancelled || now.After(p.Expires):
		a.mu.Unlock()
		writeCancelled(w)
	default:
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
	}
}

func writeCancelled(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "cancelled",
		"error":  "the account owner did not confirm this device",
	})
}

// handleDeviceConfirm issues the device's refresh token, which the poll then
// collects. It acts only on a live pending request of the signed-in account;
// anything else changes nothing and looks the same.
func (a *App) handleDeviceConfirm(w http.ResponseWriter, r *http.Request) {
	xid, ok := a.postAuth(w, r)
	if !ok {
		return
	}
	id := r.PostForm.Get("id")
	now := time.Now()
	// a.mu is held across the store call so two Confirms cannot both issue.
	// Lock order: a.mu, then store.mu.
	a.mu.Lock()
	p, found := a.pendingLinks[id]
	if found && p.XID == xid && p.State == linkPending && !now.After(p.Expires) {
		refresh, err := a.store.IssueRefreshToken(xid, p.Device)
		if err != nil {
			a.mu.Unlock()
			log.Printf("confirm device: %v", err)
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		p.State, p.Refresh = linkConfirmed, refresh
		a.pendingLinks[id] = p
	}
	a.mu.Unlock()
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// handleDeviceCancel discards a live pending request of the signed-in account.
// Nothing was issued for it, and nothing ever is.
func (a *App) handleDeviceCancel(w http.ResponseWriter, r *http.Request) {
	xid, ok := a.postAuth(w, r)
	if !ok {
		return
	}
	id := r.PostForm.Get("id")
	now := time.Now()
	a.mu.Lock()
	if p, found := a.pendingLinks[id]; found && p.XID == xid && p.State == linkPending && !now.After(p.Expires) {
		p.State = linkCancelled
		a.pendingLinks[id] = p
	}
	a.mu.Unlock()
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// pendingLinksFor lists xid's live pending requests for the account page,
// newest first.
func (a *App) pendingLinksFor(xid string, now time.Time) []pendingView {
	type row struct {
		v  pendingView
		at time.Time
	}
	var rows []row
	a.mu.Lock()
	for id, p := range a.pendingLinks {
		if p.XID != xid || p.State != linkPending || now.After(p.Expires) {
			continue
		}
		name := p.Device
		if name == "" {
			name = "NGP Phone"
		}
		rows = append(rows, row{pendingView{ID: id, Name: name, Ago: requestedAgo(now.Sub(p.Requested))}, p.Requested})
	}
	a.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].at.After(rows[j].at) })
	out := make([]pendingView, len(rows))
	for i, r := range rows {
		out[i] = r.v
	}
	return out
}

func requestedAgo(d time.Duration) string {
	if d < time.Minute {
		return "Requested just now"
	}
	return fmt.Sprintf("Requested %d min ago", int(d/time.Minute))
}

// cancelPendingLinks cancels every pending request of xid (log out, remove
// all). A confirmed one nobody has collected yet is cancelled too, and its
// device removed, so no token comes out of it.
func (a *App) cancelPendingLinks(xid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, p := range a.pendingLinks {
		if p.XID != xid || p.State == linkCancelled {
			continue
		}
		if p.State == linkConfirmed {
			if _, err := a.store.RevokeDevice(xid, hashToken(p.Refresh)); err != nil {
				log.Printf("cancel pending link: %v", err)
			}
		}
		p.State, p.Refresh = linkCancelled, ""
		a.pendingLinks[id] = p
	}
}

// reapPendingLinksLocked deletes requests pendingLinkTTL past their expiry. A
// confirmed one nobody collected has its device removed, so the device list
// keeps no device no one holds a token for. The caller holds a.mu.
func (a *App) reapPendingLinksLocked(now time.Time) {
	for id, p := range a.pendingLinks {
		if !now.After(p.Expires.Add(pendingLinkTTL)) {
			continue
		}
		if p.State == linkConfirmed {
			if _, err := a.store.RevokeDevice(p.XID, hashToken(p.Refresh)); err != nil {
				log.Printf("reap pending link: %v", err)
				continue // try again on the next sweep
			}
		}
		delete(a.pendingLinks, id)
	}
}
