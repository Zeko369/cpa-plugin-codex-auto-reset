package main

import "time"

func targetMissing(s Snapshot, id string) bool {
	for _, c := range s.Credits {
		if c.ID == id && c.Status == "available" {
			return false
		}
	}
	return true
}

func (f *AccountFSM) runtimeLocked() AccountRuntime {
	return AccountRuntime{AuthID: f.AuthID, Provider: f.Creds.Provider, Identity: f.Creds.Identity,
		State: f.state, NextWake: f.nextWake, Attempt: f.attempt, LastSnapshot: f.lastSnapshot,
		ScheduledCreditID: f.scheduledCreditID, ManualReset: f.manualReset, VerifyDue: f.verifyDue}
}

func (f *AccountFSM) Runtime() AccountRuntime {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runtimeLocked()
}

func (f *AccountFSM) Restore(r AccountRuntime) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Identity != f.Creds.Identity {
		f.state = StateHOLD
		f.nextWake = f.now()
		return
	}
	f.state = r.State
	f.nextWake = r.NextWake
	f.attempt = r.Attempt
	f.lastSnapshot = r.LastSnapshot
	f.scheduledCreditID = r.ScheduledCreditID
	f.manualReset = r.ManualReset
	f.verifyDue = r.VerifyDue
	if f.state == StateDONE || f.state == "" {
		f.resetLocked()
		f.nextWake = f.now()
	}
	if f.state == StateRESETTING && (f.attempt.RedeemRequestID == "" || f.attempt.TargetCreditID == "") {
		f.state = StateHOLD
	}
	if f.nextWake.IsZero() {
		f.nextWake = f.now()
	}
}

func (f *AccountFSM) hold(reason string) time.Time {
	f.state = StateHOLD
	next := f.now().Add(f.Cfg.RefreshInterval)
	f.log("error", reason, "read-only reconciliation; intent retained", &next, nil)
	return next
}

func (f *AccountFSM) stepHOLD() time.Time {
	snap, err := f.Client.ListCredits(f.Creds)
	if err == nil {
		f.lastSnapshot = snap
		if f.attempt.Pending && targetMissing(snap, f.attempt.TargetCreditID) && snap.AvailableCount == f.attempt.PreSnapshot.AvailableCount-1 {
			f.state = StateVERIFYING
			return f.now()
		}
	}
	return f.now().Add(f.Cfg.RefreshInterval)
}
