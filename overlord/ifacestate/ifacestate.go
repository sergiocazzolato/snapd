// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2016-2022 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

// Package ifacestate implements the manager and state aspects
// responsible for the maintenance of interfaces the system.
package ifacestate

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/i18n"
	"github.com/snapcore/snapd/interfaces"
	"github.com/snapcore/snapd/interfaces/policy"
	"github.com/snapcore/snapd/logger"
	"github.com/snapcore/snapd/overlord/assertstate"
	"github.com/snapcore/snapd/overlord/hookstate"
	"github.com/snapcore/snapd/overlord/snapstate"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/overlord/swfeats"
	"github.com/snapcore/snapd/snap"
)

var connectRetryTimeout = time.Second * 5

var regenerateSecurityProfilesChangeKind = swfeats.RegisterChangeKind("regenerate-security-profiles")

// ErrAlreadyConnected describes the error that occurs when attempting to connect already connected interface.
type ErrAlreadyConnected struct {
	Connection interfaces.ConnRef
}

const (
	ConnectTaskEdge       = state.TaskSetEdge("connect-task")
	AfterConnectHooksEdge = state.TaskSetEdge("after-connect-hooks")
)

func (e ErrAlreadyConnected) Error() string {
	return fmt.Sprintf("already connected: %q", e.Connection.ID())
}

// findSymmetricAutoconnectTask checks if there is another auto-connect task affecting same snap because of plug/slot.
func findSymmetricAutoconnectTask(st *state.State, plugSnap, slotSnap string, installTask *state.Task) (bool, error) {
	snapsup, err := snapstate.TaskSnapSetup(installTask)
	if err != nil {
		return false, fmt.Errorf("internal error: cannot obtain snap setup from task: %s", installTask.Summary())
	}
	installedSnap := snapsup.InstanceName()

	// if we find any auto-connect task that's not ready and is affecting our snap, return true to indicate that
	// it should be ignored (we shouldn't create connect tasks for it)
	for _, task := range st.Tasks() {
		if !task.Status().Ready() && task.ID() != installTask.ID() && task.Kind() == "auto-connect" {
			snapsup, err := snapstate.TaskSnapSetup(task)
			if err != nil {
				return false, fmt.Errorf("internal error: cannot obtain snap setup from task: %s", task.Summary())
			}
			otherSnap := snapsup.InstanceName()

			if (otherSnap == plugSnap && installedSnap == slotSnap) || (otherSnap == slotSnap && installedSnap == plugSnap) {
				return true, nil
			}
		}
	}
	return false, nil
}

type connectOpts struct {
	ByGadget    bool
	AutoConnect bool

	DelayedSetupProfiles bool
}

// Connect returns a set of tasks for connecting an interface.
func Connect(st *state.State, plugSnap, plugName, slotSnap, slotName string) (*state.TaskSet, error) {
	if err := snapstate.CheckChangeConflictMany(st, []string{plugSnap, slotSnap}, ""); err != nil {
		return nil, err
	}

	return connect(st, plugSnap, plugName, slotSnap, slotName, connectOpts{})
}

func connect(st *state.State, plugSnap, plugName, slotSnap, slotName string, flags connectOpts) (*state.TaskSet, error) {
	// TODO: Store the intent-to-connect in the state so that we automatically
	// try to reconnect on reboot (reconnection can fail or can connect with
	// different parameters so we cannot store the actual connection details).

	// Create a series of tasks:
	//  - prepare-plug-<plug> hook
	//  - prepare-slot-<slot> hook
	//  - connect task
	//  - connect-slot-<slot> hook
	//  - connect-plug-<plug> hook
	// The tasks run in sequence (are serialized by WaitFor). The hooks are optional
	// and their tasks are created when hook exists or is declared in the snap.
	// The prepare- hooks collect attributes via snapctl set.
	// 'snapctl set' can only modify own attributes (plug's attributes in the *-plug-* hook and
	// slot's attributes in the *-slot-* hook).
	// 'snapctl get' can read both slot's and plug's attributes.

	// check if the connection already exists
	conns, err := getConns(st)
	if err != nil {
		return nil, err
	}
	connRef := interfaces.ConnRef{PlugRef: interfaces.PlugRef{Snap: plugSnap, Name: plugName}, SlotRef: interfaces.SlotRef{Snap: slotSnap, Name: slotName}}
	if conn, ok := conns[connRef.ID()]; ok && !conn.Undesired && !conn.HotplugGone {
		return nil, &ErrAlreadyConnected{Connection: connRef}
	}

	var plugSnapst, slotSnapst snapstate.SnapState
	if err = snapstate.Get(st, plugSnap, &plugSnapst); err != nil {
		return nil, err
	}
	if err = snapstate.Get(st, slotSnap, &slotSnapst); err != nil {
		return nil, err
	}
	plugSnapInfo, err := plugSnapst.CurrentInfo()
	if err != nil {
		return nil, err
	}
	slotSnapInfo, err := slotSnapst.CurrentInfo()
	if err != nil {
		return nil, err
	}

	plugStatic, slotStatic, err := initialConnectAttributes(st, plugSnapInfo, plugSnap, plugName, slotSnapInfo, slotSnap, slotName)
	if err != nil {
		return nil, err
	}

	connectInterface := st.NewTask("connect", fmt.Sprintf(i18n.G("Connect %s:%s to %s:%s"), plugSnap, plugName, slotSnap, slotName))
	initialContext := make(map[string]any)
	initialContext["attrs-task"] = connectInterface.ID()

	tasks := state.NewTaskSet()
	var prev *state.Task
	addTask := func(t *state.Task) {
		if prev != nil {
			t.WaitFor(prev)
		}
		tasks.AddTask(t)
	}

	preparePlugHookName := fmt.Sprintf("prepare-plug-%s", plugName)
	if plugSnapInfo.Hooks[preparePlugHookName] != nil {
		plugHookSetup := &hookstate.HookSetup{
			Snap:     plugSnap,
			Hook:     preparePlugHookName,
			Optional: true,
		}
		summary := fmt.Sprintf(i18n.G("Run hook %s of snap %q"), plugHookSetup.Hook, plugHookSetup.Snap)
		undoPrepPlugHookSetup := &hookstate.HookSetup{
			Snap:        plugSnap,
			Hook:        "unprepare-plug-" + plugName,
			Optional:    true,
			IgnoreError: true,
		}
		preparePlugConnection := hookstate.HookTaskWithUndo(st, summary, plugHookSetup, undoPrepPlugHookSetup, initialContext)
		addTask(preparePlugConnection)
		prev = preparePlugConnection
	}

	prepareSlotHookName := fmt.Sprintf("prepare-slot-%s", slotName)
	if slotSnapInfo.Hooks[prepareSlotHookName] != nil {
		slotHookSetup := &hookstate.HookSetup{
			Snap:     slotSnap,
			Hook:     prepareSlotHookName,
			Optional: true,
		}
		undoPrepSlotHookSetup := &hookstate.HookSetup{
			Snap:        slotSnap,
			Hook:        "unprepare-slot-" + slotName,
			Optional:    true,
			IgnoreError: true,
		}

		summary := fmt.Sprintf(i18n.G("Run hook %s of snap %q"), slotHookSetup.Hook, slotHookSetup.Snap)
		prepareSlotConnection := hookstate.HookTaskWithUndo(st, summary, slotHookSetup, undoPrepSlotHookSetup, initialContext)
		addTask(prepareSlotConnection)
		prev = prepareSlotConnection
	}

	connectInterface.Set("slot", interfaces.SlotRef{Snap: slotSnap, Name: slotName})
	connectInterface.Set("plug", interfaces.PlugRef{Snap: plugSnap, Name: plugName})
	if flags.AutoConnect {
		connectInterface.Set("auto", true)
	}
	if flags.ByGadget {
		connectInterface.Set("by-gadget", true)
	}
	if flags.DelayedSetupProfiles {
		connectInterface.Set("delayed-setup-profiles", true)
	}

	// Expose a copy of all plug and slot attributes coming from yaml to interface hooks. The hooks will be able
	// to modify them but all attributes will be checked against assertions after the hooks are run.
	emptyDynamicAttrs := map[string]any{}
	connectInterface.Set("plug-static", plugStatic)
	connectInterface.Set("slot-static", slotStatic)
	connectInterface.Set("plug-dynamic", emptyDynamicAttrs)
	connectInterface.Set("slot-dynamic", emptyDynamicAttrs)

	// The main 'connect' task should wait on prepare-slot- hook or on prepare-plug- hook (whichever is present),
	// but not on both. While there would be no harm in waiting for both, it's not needed as prepare-slot- will
	// wait for prepare-plug- anyway, and a simple one-to-one wait dependency makes testing easier.
	addTask(connectInterface)
	prev = connectInterface

	if flags.DelayedSetupProfiles {
		// mark as the last task in connect prepare
		tasks.MarkEdge(connectInterface, ConnectTaskEdge)
	}

	connectSlotHookName := fmt.Sprintf("connect-slot-%s", slotName)
	if slotSnapInfo.Hooks[connectSlotHookName] != nil {
		connectSlotHookSetup := &hookstate.HookSetup{
			Snap:     slotSnap,
			Hook:     connectSlotHookName,
			Optional: true,
		}
		undoConnectSlotHookSetup := &hookstate.HookSetup{
			Snap:        slotSnap,
			Hook:        "disconnect-slot-" + slotName,
			Optional:    true,
			IgnoreError: true,
		}

		summary := fmt.Sprintf(i18n.G("Run hook %s of snap %q"), connectSlotHookSetup.Hook, connectSlotHookSetup.Snap)
		connectSlotConnection := hookstate.HookTaskWithUndo(st, summary, connectSlotHookSetup, undoConnectSlotHookSetup, initialContext)
		addTask(connectSlotConnection)
		prev = connectSlotConnection
		if flags.DelayedSetupProfiles {
			tasks.MarkEdge(connectSlotConnection, AfterConnectHooksEdge)
		}
	}

	connectPlugHookName := fmt.Sprintf("connect-plug-%s", plugName)
	if plugSnapInfo.Hooks[connectPlugHookName] != nil {
		connectPlugHookSetup := &hookstate.HookSetup{
			Snap:     plugSnap,
			Hook:     connectPlugHookName,
			Optional: true,
		}
		undoConnectPlugHookSetup := &hookstate.HookSetup{
			Snap:        plugSnap,
			Hook:        "disconnect-plug-" + plugName,
			Optional:    true,
			IgnoreError: true,
		}

		summary := fmt.Sprintf(i18n.G("Run hook %s of snap %q"), connectPlugHookSetup.Hook, connectPlugHookSetup.Snap)
		connectPlugConnection := hookstate.HookTaskWithUndo(st, summary, connectPlugHookSetup, undoConnectPlugHookSetup, initialContext)
		addTask(connectPlugConnection)

		if flags.DelayedSetupProfiles {
			// only mark AfterConnectHooksEdge if not already set on connect-slot- hook task
			if edge, _ := tasks.Edge(AfterConnectHooksEdge); edge == nil {
				tasks.MarkEdge(connectPlugConnection, AfterConnectHooksEdge)
			}
		}
		prev = connectPlugConnection
	}
	return tasks, nil
}

func initialConnectAttributes(st *state.State, plugSnapInfo *snap.Info, plugSnap string, plugName string, slotSnapInfo *snap.Info, slotSnap string, slotName string) (plugStatic, slotStatic map[string]any, err error) {
	var plugSnapst snapstate.SnapState

	if err = snapstate.Get(st, plugSnap, &plugSnapst); err != nil {
		return nil, nil, err
	}

	if err := addImplicitInterfaces(st, plugSnapInfo); err != nil {
		return nil, nil, err
	}

	plug, ok := plugSnapInfo.Plugs[plugName]
	if !ok {
		return nil, nil, fmt.Errorf("snap %q has no plug named %q", plugSnap, plugName)
	}

	var slotSnapst snapstate.SnapState

	if err = snapstate.Get(st, slotSnap, &slotSnapst); err != nil {
		return nil, nil, err
	}

	if err := addImplicitInterfaces(st, slotSnapInfo); err != nil {
		return nil, nil, err
	}

	slot, ok := slotSnapInfo.Slots[slotName]
	if !ok {
		return nil, nil, fmt.Errorf("snap %q has no slot named %q", slotSnap, slotName)
	}

	return plug.Attrs, slot.Attrs, nil
}

// Disconnect returns a set of tasks for disconnecting an interface.
func Disconnect(st *state.State, conn *interfaces.Connection) (*state.TaskSet, error) {
	plugSnap := conn.Plug.Snap().InstanceName()
	slotSnap := conn.Slot.Snap().InstanceName()
	if err := snapstate.CheckChangeConflictMany(st, []string{plugSnap, slotSnap}, ""); err != nil {
		return nil, err
	}

	return disconnectTasks(st, conn, disconnectOpts{})
}

// Forget returs a set of tasks for disconnecting and forgetting an interface.
// If the interface is already disconnected, it will be removed from the state
// (forgotten).
func Forget(st *state.State, repo *interfaces.Repository, connRef *interfaces.ConnRef) (*state.TaskSet, error) {
	if err := snapstate.CheckChangeConflictMany(st, []string{connRef.PlugRef.Snap, connRef.SlotRef.Snap}, ""); err != nil {
		return nil, err
	}

	if conn, err := repo.Connection(connRef); err == nil {
		// connection exists - run regular set of disconnect tasks with forget
		// flag.
		opts := disconnectOpts{Forget: true}
		ts, err := disconnectTasks(st, conn, opts)
		return ts, err
	}

	// connection is not active (and possibly either the plug or slot
	// doesn't exist); disconnect tasks don't need hooks as we simply
	// want to remove connection from state.
	ts := forgetTasks(st, connRef)
	return ts, nil
}

type disconnectOpts struct {
	AutoDisconnect bool
	ByHotplug      bool
	Forget         bool
}

// forgetTasks creates a set of tasks for forgetting an inactive connection
func forgetTasks(st *state.State, connRef *interfaces.ConnRef) *state.TaskSet {
	summary := fmt.Sprintf(i18n.G("Forget connection %s:%s from %s:%s"),
		connRef.PlugRef.Snap, connRef.PlugRef.Name,
		connRef.SlotRef.Snap, connRef.SlotRef.Name)
	disconnectTask := st.NewTask("disconnect", summary)
	disconnectTask.Set("slot", connRef.SlotRef)
	disconnectTask.Set("plug", connRef.PlugRef)
	disconnectTask.Set("forget", true)
	return state.NewTaskSet(disconnectTask)
}

// disconnectTasks creates a set of tasks for disconnect, including hooks, but does not do any conflict checking.
func disconnectTasks(st *state.State, conn *interfaces.Connection, flags disconnectOpts) (*state.TaskSet, error) {
	plugSnap := conn.Plug.Snap().InstanceName()
	slotSnap := conn.Slot.Snap().InstanceName()
	plugName := conn.Plug.Name()
	slotName := conn.Slot.Name()

	var plugSnapst, slotSnapst snapstate.SnapState
	if err := snapstate.Get(st, slotSnap, &slotSnapst); err != nil {
		return nil, err
	}
	if err := snapstate.Get(st, plugSnap, &plugSnapst); err != nil {
		return nil, err
	}

	summary := fmt.Sprintf(i18n.G("Disconnect %s:%s from %s:%s"),
		plugSnap, plugName, slotSnap, slotName)
	disconnectTask := st.NewTask("disconnect", summary)
	disconnectTask.Set("slot", interfaces.SlotRef{Snap: slotSnap, Name: slotName})
	disconnectTask.Set("plug", interfaces.PlugRef{Snap: plugSnap, Name: plugName})
	if flags.Forget {
		disconnectTask.Set("forget", true)
	}

	disconnectTask.Set("slot-static", conn.Slot.StaticAttrs())
	disconnectTask.Set("slot-dynamic", conn.Slot.DynamicAttrs())
	disconnectTask.Set("plug-static", conn.Plug.StaticAttrs())
	disconnectTask.Set("plug-dynamic", conn.Plug.DynamicAttrs())

	if flags.AutoDisconnect {
		disconnectTask.Set("auto-disconnect", true)
	}
	if flags.ByHotplug {
		disconnectTask.Set("by-hotplug", true)
	}

	ts := state.NewTaskSet()
	var prev *state.Task
	addTask := func(t *state.Task) {
		if prev != nil {
			t.WaitFor(prev)
		}
		ts.AddTask(t)
		prev = t
	}

	initialContext := make(map[string]any)
	initialContext["attrs-task"] = disconnectTask.ID()

	plugSnapInfo, err := plugSnapst.CurrentInfo()
	if err != nil {
		return nil, err
	}
	slotSnapInfo, err := slotSnapst.CurrentInfo()
	if err != nil {
		return nil, err
	}

	// only run slot hooks if slotSnap is active
	if slotSnapst.Active {
		hookName := fmt.Sprintf("disconnect-slot-%s", slotName)
		if slotSnapInfo.Hooks[hookName] != nil {
			disconnectSlotHookSetup := &hookstate.HookSetup{
				Snap:        slotSnap,
				Hook:        hookName,
				Optional:    true,
				IgnoreError: flags.AutoDisconnect,
			}
			undoDisconnectSlotHookSetup := &hookstate.HookSetup{
				Snap:        slotSnap,
				Hook:        "connect-slot-" + slotName,
				Optional:    true,
				IgnoreError: flags.AutoDisconnect,
			}

			summary := fmt.Sprintf(i18n.G("Run hook %s of snap %q"), disconnectSlotHookSetup.Hook, disconnectSlotHookSetup.Snap)
			disconnectSlot := hookstate.HookTaskWithUndo(st, summary, disconnectSlotHookSetup, undoDisconnectSlotHookSetup, initialContext)

			addTask(disconnectSlot)
		}
	}

	// only run plug hooks if plugSnap is active
	if plugSnapst.Active {
		hookName := fmt.Sprintf("disconnect-plug-%s", plugName)
		if plugSnapInfo.Hooks[hookName] != nil {
			disconnectPlugHookSetup := &hookstate.HookSetup{
				Snap:        plugSnap,
				Hook:        hookName,
				Optional:    true,
				IgnoreError: flags.AutoDisconnect,
			}
			undoDisconnectPlugHookSetup := &hookstate.HookSetup{
				Snap:        plugSnap,
				Hook:        "connect-plug-" + plugName,
				Optional:    true,
				IgnoreError: flags.AutoDisconnect,
			}

			summary := fmt.Sprintf(i18n.G("Run hook %s of snap %q"), disconnectPlugHookSetup.Hook, disconnectPlugHookSetup.Snap)
			disconnectPlug := hookstate.HookTaskWithUndo(st, summary, disconnectPlugHookSetup, undoDisconnectPlugHookSetup, initialContext)

			addTask(disconnectPlug)
		}
	}

	addTask(disconnectTask)
	return ts, nil
}

// CheckInterfaces checks whether plugs and slots of snap are allowed for installation.
func CheckInterfaces(st *state.State, snapInfo *snap.Info, deviceCtx snapstate.DeviceContext) error {
	// XXX: addImplicitSlots is really a brittle interface
	if err := addImplicitInterfaces(st, snapInfo); err != nil {
		return err
	}

	modelAs := deviceCtx.Model()

	var storeAs *asserts.Store
	if modelAs.Store() != "" {
		var err error
		storeAs, err = assertstate.Store(st, modelAs.Store())
		if err != nil && !errors.Is(err, &asserts.NotFoundError{}) {
			return err
		}
	}

	baseDecl, err := assertstate.BaseDeclaration(st)
	if err != nil {
		return fmt.Errorf("internal error: cannot find base declaration: %v", err)
	}

	if snapInfo.SnapID == "" {
		// no SnapID means --dangerous was given, perform a minimal check about the compatibility of the snap type and the interface
		ic := policy.InstallCandidateMinimalCheck{
			Snap:            snapInfo,
			BaseDeclaration: baseDecl,
			Model:           modelAs,
			Store:           storeAs,
		}
		return ic.Check()
	}

	snapDecl, err := assertstate.SnapDeclaration(st, snapInfo.SnapID)
	if err != nil {
		return fmt.Errorf("cannot find snap declaration for %q: %v", snapInfo.InstanceName(), err)
	}

	ic := policy.InstallCandidate{
		Snap:            snapInfo,
		SnapDeclaration: snapDecl,
		BaseDeclaration: baseDecl,
		Model:           modelAs,
		Store:           storeAs,
	}

	return ic.Check()
}

var once sync.Once

func delayedCrossMgrInit() {
	once.Do(func() {
		// hook interface checks into snapstate installation logic

		snapstate.AddCheckSnapCallback(func(st *state.State, snapInfo, _ *snap.Info, _ snap.Container, _ snapstate.Flags, deviceCtx snapstate.DeviceContext) error {
			return CheckInterfaces(st, snapInfo, deviceCtx)
		})

		// hook into conflict checks mechanisms
		snapstate.RegisterAffectedSnapsByKind("connect", connectDisconnectAffectedSnaps)
		snapstate.RegisterAffectedSnapsByKind("disconnect", connectDisconnectAffectedSnaps)

		// hook into snap linking/unlinking and activation state changes
		snapstate.AddLinkSnapParticipant(snapstate.LinkSnapParticipantFunc(OnSnapLinkageChanged))
	})
}

func MockConnectRetryTimeout(d time.Duration) (restore func()) {
	old := connectRetryTimeout
	connectRetryTimeout = d
	return func() { connectRetryTimeout = old }
}

// OnSnapLinkageChanged is used to implement
// snapstate.LinkSnapParticipant follow activation changes for snaps
// so that we can track revisions with security profiles on disk for
// temporarily inactive snaps.
func OnSnapLinkageChanged(st *state.State, snapsup *snapstate.SnapSetup) error {
	instanceName := snapsup.InstanceName()

	var snapst snapstate.SnapState
	if err := snapstate.Get(st, instanceName, &snapst); err != nil && !errors.Is(err, state.ErrNoState) {
		return err
	}
	if !snapst.IsInstalled() {
		// nothing to do
		return nil
	}

	if snapst.Active {
		// nothing to track
		snapst.PendingSecurity = nil
	} else {
		// track the revision that was just unlinked that has
		// still profiles
		snapst.PendingSecurity = &snapstate.PendingSecurityState{
			SideInfo:   snapst.CurrentSideInfo(),
			Components: snapst.CurrentComponentSideInfos(),
		}
	}
	snapstate.Set(st, instanceName, &snapst)
	return nil
}

// InterfacesRequestsControlHandlerServices returns the list of all apps which
// are defined as "handler-service" for a snap which has a connected plug for
// the "snap-interfaces-requests-control" interface.
//
// The caller must ensure that the given state is locked.
func InterfacesRequestsControlHandlerServices(st *state.State) ([]*snap.AppInfo, error) {
	conns, err := ConnectionStates(st)
	if err != nil {
		return nil, fmt.Errorf("internal error: cannot get connections: %w", err)
	}

	var handlers []*snap.AppInfo

	for connId, connState := range conns {
		if connState.Interface != "snap-interfaces-requests-control" || !connState.Active() {
			continue
		}

		connRef, err := interfaces.ParseConnRef(connId)
		if err != nil {
			return nil, err
		}

		handler, ok := connState.StaticPlugAttrs["handler-service"].(string)
		if !ok {
			// does not have a handler service
			continue
		}

		sn := connRef.PlugRef.Snap
		si, err := snapstate.CurrentInfo(st, sn)
		if err != nil {
			return nil, err
		}

		// this should not fail as the plug's BeforePrepare should have validated that such an app exists
		app := si.Apps[handler]
		if app == nil {
			return nil, fmt.Errorf("internal error: cannot find app %q in snap %q", app, sn)
		}

		handlers = append(handlers, app)
	}

	return handlers, nil
}

// AdviseReportedSystemKeyMismatch inspects the system key, which is reportedly
// in a mismatch with the recoded one, and decides to either create a state
// change for regenerating security profiles, thus returning a change, or do
// nothing, in which case no change is returned.
func AdviseReportedSystemKeyMismatch(st *state.State, systemKey any) (*state.Change, error) {
	var seeded bool
	err := st.Get("seeded", &seeded)
	if err != nil && !errors.Is(err, state.ErrNoState) {
		return nil, err
	}

	if !seeded {
		// System not ready yet for checking system-key, bubble up the error to
		// clients. They can either wait or proceed at their own peril.
		return nil, errors.New("system not yet seeded")
	}

	action, err := interfaces.SystemKeyMismatchAdvice(systemKey)
	if err != nil {
		return nil, err
	}

	logger.Debugf("reported system key mismatch action: %v", action)

	if action == interfaces.SystemKeyMismatchActionNone {
		// nothing to do
		return nil, nil
	}

	for _, chg := range st.Changes() {
		// if we have a change that isn't ready, return it instead
		if chg.Kind() == regenerateSecurityProfilesChangeKind && !chg.IsReady() {
			return chg, nil
		}
	}

	chg := st.NewChange(regenerateSecurityProfilesChangeKind, "Regenerate security profiles")
	t := st.NewTask(regenerateSecurityProfilesChangeKind, "Regenerate security profiles")
	chg.AddTask(t)

	return chg, nil
}
