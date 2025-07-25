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

package snapstate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	. "gopkg.in/check.v1"
	"gopkg.in/tomb.v2"

	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/boot"
	"github.com/snapcore/snapd/boot/boottest"
	"github.com/snapcore/snapd/bootloader"
	"github.com/snapcore/snapd/bootloader/bootloadertest"
	"github.com/snapcore/snapd/cmd/snaplock"
	"github.com/snapcore/snapd/cmd/snaplock/runinhibit"
	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/overlord/auth"
	"github.com/snapcore/snapd/overlord/configstate/config"
	"github.com/snapcore/snapd/overlord/ifacestate"
	"github.com/snapcore/snapd/overlord/restart"
	"github.com/snapcore/snapd/overlord/servicestate"
	"github.com/snapcore/snapd/overlord/snapstate"
	"github.com/snapcore/snapd/overlord/snapstate/backend"
	"github.com/snapcore/snapd/overlord/snapstate/sequence"
	"github.com/snapcore/snapd/overlord/snapstate/snapstatetest"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/release"
	"github.com/snapcore/snapd/sandbox/apparmor"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/snap/naming"
	"github.com/snapcore/snapd/snap/snaptest"
	"github.com/snapcore/snapd/snapdtool"
	"github.com/snapcore/snapd/testutil"
	userclient "github.com/snapcore/snapd/usersession/client"
)

type linkSnapSuite struct {
	baseHandlerSuite

	restartRequested []restart.RestartType
}

var _ = Suite(&linkSnapSuite{})

func (s *linkSnapSuite) SetUpTest(c *C) {
	s.baseHandlerSuite.SetUpTest(c)

	s.state.Lock()
	_, err := restart.Manager(s.state, "boot-id-0", snapstatetest.MockRestartHandler(func(t restart.RestartType) {
		s.restartRequested = append(s.restartRequested, t)
	}))
	s.state.Unlock()
	c.Assert(err, IsNil)

	s.AddCleanup(snapstatetest.MockDeviceModel(DefaultModel()))

	oldSnapServiceOptions := snapstate.SnapServiceOptions
	snapstate.SnapServiceOptions = servicestate.SnapServiceOptions
	s.AddCleanup(func() {
		snapstate.SnapServiceOptions = oldSnapServiceOptions
		s.restartRequested = nil
	})

	s.AddCleanup(snapstate.MockLinkSnapParticipants([]snapstate.LinkSnapParticipant{snapstate.LinkSnapParticipantFunc(ifacestate.OnSnapLinkageChanged)}))
}

func checkHasCookieForSnap(c *C, st *state.State, instanceName string) {
	var contexts map[string]any
	err := st.Get("snap-cookies", &contexts)
	c.Assert(err, IsNil)
	c.Check(contexts, HasLen, 1)

	for _, snap := range contexts {
		if instanceName == snap {
			return
		}
	}
	panic(fmt.Sprintf("Cookie missing for snap %q", instanceName))
}

func (s *linkSnapSuite) TestDoLinkSnapSuccess(c *C) {
	// we start without the auxiliary store info
	c.Check(backend.AuxStoreInfoFilename("foo-id"), testutil.FileAbsent)

	lp := &testLinkParticipant{}
	restore := snapstate.MockLinkSnapParticipants([]snapstate.LinkSnapParticipant{lp, snapstate.LinkSnapParticipantFunc(ifacestate.OnSnapLinkageChanged)})
	defer restore()

	s.state.Lock()
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: &snap.SideInfo{
			RealName: "foo",
			Revision: snap.R(33),
			SnapID:   "foo-id",
		},
		Channel: "beta",
		UserID:  2,
	})
	s.state.NewChange("sample", "...").AddTask(t)

	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()
	defer s.state.Unlock()
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "foo", &snapst)
	c.Assert(err, IsNil)

	checkHasCookieForSnap(c, s.state, "foo")

	typ, err := snapst.Type()
	c.Check(err, IsNil)
	c.Check(typ, Equals, snap.TypeApp)

	c.Check(snapst.Active, Equals, true)
	c.Check(snapst.Sequence.Revisions, HasLen, 1)
	c.Check(snapst.Current, Equals, snap.R(33))
	c.Check(snapst.TrackingChannel, Equals, "latest/beta")
	c.Check(snapst.UserID, Equals, 2)
	c.Check(snapst.CohortKey, Equals, "")
	c.Check(t.Status(), Equals, state.DoneStatus)
	c.Check(s.restartRequested, HasLen, 0)

	// we end with the auxiliary store info
	c.Check(backend.AuxStoreInfoFilename("foo-id"), testutil.FilePresent)

	// link snap participant was invoked
	c.Check(lp.instanceNames, DeepEquals, []string{"foo"})
}

func (s *linkSnapSuite) TestDoLinkSnapSuccessWithCohort(c *C) {
	// we start without the auxiliary store info
	c.Check(backend.AuxStoreInfoFilename("foo-id"), testutil.FileAbsent)

	s.state.Lock()
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: &snap.SideInfo{
			RealName: "foo",
			Revision: snap.R(33),
			SnapID:   "foo-id",
		},
		Channel:   "beta",
		UserID:    2,
		CohortKey: "wobbling",
	})
	s.state.NewChange("sample", "...").AddTask(t)

	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()
	defer s.state.Unlock()
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "foo", &snapst)
	c.Assert(err, IsNil)

	checkHasCookieForSnap(c, s.state, "foo")

	typ, err := snapst.Type()
	c.Check(err, IsNil)
	c.Check(typ, Equals, snap.TypeApp)

	c.Check(snapst.Active, Equals, true)
	c.Check(snapst.Sequence.Revisions, HasLen, 1)
	c.Check(snapst.Current, Equals, snap.R(33))
	c.Check(snapst.TrackingChannel, Equals, "latest/beta")
	c.Check(snapst.UserID, Equals, 2)
	c.Check(snapst.CohortKey, Equals, "wobbling")
	c.Check(t.Status(), Equals, state.DoneStatus)
	c.Check(s.restartRequested, HasLen, 0)

	// we end with the auxiliary store info
	c.Check(backend.AuxStoreInfoFilename("foo-id"), testutil.FilePresent)
}

func (s *linkSnapSuite) TestDoLinkSnapSuccessNoUserID(c *C) {
	s.state.Lock()
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: &snap.SideInfo{
			RealName: "foo",
			Revision: snap.R(33),
		},
		Channel: "beta",
	})
	s.state.NewChange("sample", "...").AddTask(t)

	s.state.Unlock()
	s.se.Ensure()
	s.se.Wait()
	s.state.Lock()
	defer s.state.Unlock()

	// check that snapst.UserID does not get set
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "foo", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.UserID, Equals, 0)

	var snaps map[string]*json.RawMessage
	err = s.state.Get("snaps", &snaps)
	c.Assert(err, IsNil)
	raw := []byte(*snaps["foo"])
	c.Check(string(raw), Not(testutil.Contains), "user-id")
}

func (s *linkSnapSuite) TestDoLinkSnapSuccessUserIDAlreadySet(c *C) {
	s.state.Lock()
	snapstate.Set(s.state, "foo", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
			{RealName: "foo", Revision: snap.R(1)},
		}),
		Current: snap.R(1),
		UserID:  1,
	})
	// the user
	user, err := auth.NewUser(s.state, auth.NewUserParams{
		Username:   "username",
		Email:      "email@test.com",
		Macaroon:   "macaroon",
		Discharges: []string{"discharge"},
	})
	c.Assert(err, IsNil)
	c.Assert(user.ID, Equals, 1)

	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: &snap.SideInfo{
			RealName: "foo",
			Revision: snap.R(33),
		},
		Channel: "beta",
		UserID:  2,
	})
	s.state.NewChange("sample", "...").AddTask(t)

	s.state.Unlock()
	s.se.Ensure()
	s.se.Wait()
	s.state.Lock()
	defer s.state.Unlock()

	// check that snapst.UserID was not "transferred"
	var snapst snapstate.SnapState
	err = snapstate.Get(s.state, "foo", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.UserID, Equals, 1)
}

func (s *linkSnapSuite) TestDoLinkSnapSuccessUserLoggedOut(c *C) {
	s.state.Lock()
	snapstate.Set(s.state, "foo", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
			{RealName: "foo", Revision: snap.R(1)},
		}),
		Current: snap.R(1),
		UserID:  1,
	})

	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: &snap.SideInfo{
			RealName: "foo",
			Revision: snap.R(33),
		},
		Channel: "beta",
		UserID:  2,
	})
	s.state.NewChange("sample", "...").AddTask(t)

	s.state.Unlock()
	s.se.Ensure()
	s.se.Wait()
	s.state.Lock()
	defer s.state.Unlock()

	// check that snapst.UserID was transferred
	// given that user 1 doesn't exist anymore
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "foo", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.UserID, Equals, 2)
}

func (s *linkSnapSuite) TestDoLinkSnapSeqFile(c *C) {
	s.state.Lock()
	// pretend we have an installed snap
	si11 := &snap.SideInfo{
		RealName: "foo",
		Revision: snap.R(11),
	}
	snapstate.Set(s.state, "foo", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si11}),
		Current:  si11.Revision,
	})
	// add a new one
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: &snap.SideInfo{
			RealName: "foo",
			Revision: snap.R(33),
		},
		Channel: "beta",
	})
	s.state.NewChange("sample", "...").AddTask(t)
	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()
	defer s.state.Unlock()
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "foo", &snapst)
	c.Assert(err, IsNil)

	// and check that the sequence file got updated
	seqContent, err := os.ReadFile(filepath.Join(dirs.SnapSeqDir, "foo.json"))
	c.Assert(err, IsNil)
	c.Check(string(seqContent), Equals, `{"sequence":[{"name":"foo","snap-id":"","revision":"11"},{"name":"foo","snap-id":"","revision":"33"}],"current":"33","migrated-hidden":false,"migrated-exposed-home":false}`)
}

func (s *linkSnapSuite) TestDoUndoLinkSnap(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	linkChangeCount := 0
	lp := &testLinkParticipant{
		linkageChanged: func(st *state.State, snapsup *snapstate.SnapSetup) error {
			var snapst snapstate.SnapState
			err := snapstate.Get(st, snapsup.InstanceName(), &snapst)
			linkChangeCount++
			switch linkChangeCount {
			case 1:
				// Initially the snap gets linked.
				c.Check(err, IsNil)
				c.Check(snapst.Active, Equals, true)
			case 2:
				// Then link-snap is undone and the snap gets unlinked.
				c.Check(err, testutil.ErrorIs, state.ErrNoState)
			}
			return nil
		},
	}
	restore := snapstate.MockLinkSnapParticipants([]snapstate.LinkSnapParticipant{lp, snapstate.LinkSnapParticipantFunc(ifacestate.OnSnapLinkageChanged)})
	defer restore()

	// a hook might have set some config
	cfg := json.RawMessage(`{"c":true}`)
	err := config.SetSnapConfig(s.state, "foo", &cfg)
	c.Assert(err, IsNil)

	si := &snap.SideInfo{
		RealName: "foo",
		Revision: snap.R(33),
	}
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Channel:  "beta",
	})
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(t)

	terr := s.state.NewTask("error-trigger", "provoking total undo")
	terr.WaitFor(t)
	chg.AddTask(terr)

	s.state.Unlock()

	for i := 0; i < 6; i++ {
		s.se.Ensure()
		s.se.Wait()
	}

	s.state.Lock()
	var snapst snapstate.SnapState
	err = snapstate.Get(s.state, "foo", &snapst)
	c.Assert(err, testutil.ErrorIs, state.ErrNoState)
	c.Check(t.Status(), Equals, state.UndoneStatus)

	// and check that the sequence file got updated
	seqContent, err := os.ReadFile(filepath.Join(dirs.SnapSeqDir, "foo.json"))
	c.Assert(err, IsNil)
	c.Check(string(seqContent), Equals, `{"sequence":[],"current":"unset","migrated-hidden":false,"migrated-exposed-home":false}`)

	// nothing in config
	var config map[string]*json.RawMessage
	err = s.state.Get("config", &config)
	c.Assert(err, IsNil)
	c.Check(config, HasLen, 1)
	_, ok := config["core"]
	c.Check(ok, Equals, true)

	// link snap participant was invoked, once for do, once for undo.
	c.Check(lp.instanceNames, DeepEquals, []string{"foo", "foo"})
}

func (s *linkSnapSuite) TestDoUnlinkCurrentSnapWithIgnoreRunning(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// With refresh-app-awareness enabled
	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.refresh-app-awareness", true)
	tr.Commit()

	// With a snap "pkg" at revision 42
	si := &snap.SideInfo{RealName: "pkg", Revision: snap.R(42)}
	snapstate.Set(s.state, "pkg", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
		Current:  si.Revision,
		Active:   true,
	})

	// With an app belonging to the snap that is apparently running.
	snapstate.MockSnapReadInfo(func(name string, si *snap.SideInfo) (*snap.Info, error) {
		c.Assert(name, Equals, "pkg")
		info := &snap.Info{SuggestedName: name, SideInfo: *si, SnapType: snap.TypeApp}
		info.Apps = map[string]*snap.AppInfo{
			"app": {Snap: info, Name: "app"},
		}
		return info, nil
	})
	restore := snapstate.MockPidsOfSnap(func(instanceName string) (map[string][]int, error) {
		c.Assert(instanceName, Equals, "pkg")
		return map[string][]int{"snap.pkg.app": {1234}}, nil
	})
	defer restore()

	var called bool
	restore = snapstate.MockExcludeFromRefreshAppAwareness(func(t snap.Type) bool {
		called = true
		c.Check(t, Equals, snap.TypeApp)
		return false
	})
	defer restore()

	// We can unlink the current revision of that snap, by setting IgnoreRunning flag.
	task := s.state.NewTask("unlink-current-snap", "")
	task.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Flags:    snapstate.Flags{IgnoreRunning: true},
		Type:     "app",
	})
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(task)

	// Run the task we created
	s.state.Unlock()
	s.se.Ensure()
	s.se.Wait()
	s.state.Lock()

	// And observe the results.
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "pkg", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.Active, Equals, false)
	c.Check(snapst.Sequence.Revisions, HasLen, 1)
	c.Check(snapst.Current, Equals, snap.R(42))
	c.Check(task.Status(), Equals, state.DoneStatus)
	expected := fakeOps{{
		op:   "unlink-snap",
		path: filepath.Join(dirs.SnapMountDir, "pkg/42"),
	}}
	c.Check(s.fakeBackend.ops, DeepEquals, expected)
	c.Check(called, Equals, true)
}

func (s *linkSnapSuite) TestDoUnlinkCurrentSnapWithKernelModulesComponents(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.refresh-app-awareness", true)
	tr.Commit()

	// With a snap "pkg" at revision 42
	si := &snap.SideInfo{RealName: "pkg", Revision: snap.R(42)}

	// we add a kernel module component so that we make sure the task attaches
	// it to the SnapSetup for use in later tasks
	kmodCsi := snap.ComponentSideInfo{
		Component: naming.NewComponentRef("pkg", "kmod-comp"),
		Revision:  snap.R(10),
	}

	seq := snapstatetest.NewSequenceFromRevisionSideInfos([]*sequence.RevisionSideState{
		sequence.NewRevisionSideState(si, []*sequence.ComponentState{
			sequence.NewComponentState(&kmodCsi, snap.KernelModulesComponent),
			sequence.NewComponentState(&snap.ComponentSideInfo{
				Component: naming.NewComponentRef("pkg", "comp"),
				Revision:  snap.R(11),
			}, snap.StandardComponent),
		}),
	})

	snapstate.Set(s.state, "pkg", &snapstate.SnapState{
		Sequence: seq,
		Current:  si.Revision,
		Active:   true,
	})

	snapstate.MockSnapReadInfo(func(name string, si *snap.SideInfo) (*snap.Info, error) {
		c.Assert(name, Equals, "pkg")
		info := &snap.Info{SuggestedName: name, SideInfo: *si, SnapType: snap.TypeApp}
		info.Apps = map[string]*snap.AppInfo{
			"app": {Snap: info, Name: "app"},
		}
		return info, nil
	})

	task := s.state.NewTask("unlink-current-snap", "")
	task.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Type:     "app",
	})
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(task)

	// Run the task we created
	s.state.Unlock()
	s.se.Ensure()
	s.se.Wait()
	s.state.Lock()

	// And observe the results.
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "pkg", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.Active, Equals, false)
	c.Check(snapst.Sequence.Revisions, HasLen, 1)
	c.Check(snapst.Current, Equals, snap.R(42))
	c.Check(task.Status(), Equals, state.DoneStatus)
	expected := fakeOps{
		{
			op:          "run-inhibit-snap-for-unlink",
			name:        "pkg",
			inhibitHint: "refresh",
		},
		{
			op:   "unlink-snap",
			path: filepath.Join(dirs.SnapMountDir, "pkg/42"),
		},
	}
	c.Check(s.fakeBackend.ops, DeepEquals, expected)

	var snapsup snapstate.SnapSetup
	err = task.Get("snap-setup", &snapsup)
	c.Assert(err, IsNil)
	c.Check(snapsup.PreUpdateKernelModuleComponents, DeepEquals, []*snap.ComponentSideInfo{&kmodCsi})
}

func (s *linkSnapSuite) TestDoUnlinkCurrentSnapSnapLockUnlocked(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// Make sure refresh-app-awareness is enabled
	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.refresh-app-awareness", true)
	tr.Commit()

	instant := time.Now()
	pastInstant := instant.Add(-snapstate.MaxInhibitionDuration(s.state) * 2)
	// Add test snap
	si := &snap.SideInfo{RealName: "pkg", Revision: snap.R(42)}
	snaptest.MockSnap(c, `name: pkg`, si)
	snapstate.Set(s.state, "pkg", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
		Current:  si.Revision,
		Active:   true,
		// Pretend inhibition is overdue.
		RefreshInhibitedTime: &pastInstant,
	})

	restore := snapstate.MockAsyncPendingRefreshNotification(func(_ context.Context, pendingInfo *userclient.PendingSnapRefreshInfo) {})
	defer restore()

	var appCheckCalled int
	restore = snapstate.MockRefreshAppsCheck(func(info *snap.Info) error {
		appCheckCalled++
		return snapstate.NewBusySnapError(info, []int{123}, nil, nil)
	})
	defer restore()

	restore = snapstate.MockOnRefreshInhibitionTimeout(func(chg *state.Change, snapName string) error {
		return fmt.Errorf("boom!")
	})
	defer restore()

	task := s.state.NewTask("unlink-current-snap", "")
	task.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Type:     snap.TypeApp,
		Flags:    snapstate.Flags{IsAutoRefresh: true},
	})
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(task)

	// Run the task we created
	s.state.Unlock()
	s.se.Ensure()
	s.se.Wait()
	s.state.Lock()

	// And observe the results.
	c.Check(task.Status(), Equals, state.ErrorStatus)
	expected := fakeOps{{
		op:          "run-inhibit-snap-for-unlink",
		name:        "pkg",
		inhibitHint: "refresh",
	}}
	c.Check(s.fakeBackend.ops, DeepEquals, expected)
	c.Check(appCheckCalled, Equals, 1)

	// snap lock should be unlocked
	lock, err := osutil.NewFileLock(filepath.Join(s.fakeBackend.lockDir, "pkg.lock"))
	c.Assert(err, IsNil)
	defer lock.Close()
	c.Assert(lock.TryLock(), IsNil)
}

func (s *linkSnapSuite) TestDoUndoUnlinkCurrentSnapWithVitalityScore(c *C) {
	s.state.Lock()
	defer s.state.Unlock()
	// foo has a vitality-hint
	cfg := json.RawMessage(`{"resilience":{"vitality-hint":"bar,foo,baz"}}`)
	err := config.SetSnapConfig(s.state, "core", &cfg)
	c.Assert(err, IsNil)

	si1 := &snap.SideInfo{
		RealName: "foo",
		Revision: snap.R(11),
	}
	si2 := &snap.SideInfo{
		RealName: "foo",
		Revision: snap.R(33),
	}
	snapstate.Set(s.state, "foo", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si1}),
		Current:  si1.Revision,
		Active:   true,
	})
	t := s.state.NewTask("unlink-current-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si2,
	})
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(t)

	terr := s.state.NewTask("error-trigger", "provoking total undo")
	terr.WaitFor(t)
	chg.AddTask(terr)

	s.state.Unlock()

	for i := 0; i < 3; i++ {
		s.se.Ensure()
		s.se.Wait()
	}

	s.state.Lock()
	var snapst snapstate.SnapState
	err = snapstate.Get(s.state, "foo", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.Active, Equals, true)
	c.Check(snapst.Sequence.Revisions, HasLen, 1)
	c.Check(snapst.Current, Equals, snap.R(11))
	c.Check(t.Status(), Equals, state.UndoneStatus)

	expected := fakeOps{
		{
			op:          "run-inhibit-snap-for-unlink",
			name:        "foo",
			inhibitHint: "refresh",
		},
		{
			op:   "unlink-snap",
			path: filepath.Join(dirs.SnapMountDir, "foo/11"),
		},
		{
			op:           "link-snap",
			path:         filepath.Join(dirs.SnapMountDir, "foo/11"),
			vitalityRank: 2,
		},
		{
			op:     "maybe-set-next-boot",
			isUndo: true,
		},
	}
	c.Check(s.fakeBackend.ops, DeepEquals, expected)
}

func (s *linkSnapSuite) TestDoUnlinkCurrentSnapSnapdNop(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	si := &snap.SideInfo{
		RealName: "snapd",
		SnapID:   "snapd-snap-id",
		Revision: snap.R(22),
	}
	siOld := *si
	siOld.Revision = snap.R(20)
	snapstate.Set(s.state, "snapd", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{&siOld}),
		Current:  siOld.Revision,
		Active:   true,
		SnapType: "snapd",
	})

	task := s.state.NewTask("unlink-current-snap", "")
	task.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Channel:  "beta",
	})
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(task)

	// Run the task we created
	s.state.Unlock()
	s.se.Ensure()
	s.se.Wait()
	s.state.Lock()

	// And observe the results.
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "snapd", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.Active, Equals, false)
	c.Check(snapst.Sequence.Revisions, HasLen, 1)
	c.Check(snapst.Current, Equals, snap.R(20))
	c.Check(task.Status(), Equals, state.DoneStatus)
	// backend unlink was not called
	c.Check(s.fakeBackend.ops, HasLen, 1)
	c.Check(s.fakeBackend.ops, DeepEquals, fakeOps{
		{
			op:          "run-inhibit-snap-for-unlink",
			name:        "snapd",
			inhibitHint: "refresh",
		}})
}

func (s *linkSnapSuite) TestDoUnlinkCurrentSnapNoRestartSnapd(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	s.runner.AddHandler("failure-task", func(_ *state.Task, _ *tomb.Tomb) error {
		return fmt.Errorf("oh no!")
	}, nil)

	si := &snap.SideInfo{
		RealName: "snapd",
		Revision: snap.R(20),
	}
	snapstate.Set(s.state, "snapd", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
		Current:  si.Revision,
		Active:   true,
	})

	chg := s.state.NewChange("sample", "...")

	raTask := s.state.NewTask("remove-aliases", "")
	raTask.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Channel:  "beta",
	})
	chg.AddTask(raTask)

	unlinkTask := s.state.NewTask("unlink-current-snap", "")
	unlinkTask.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Channel:  "beta",
	})
	unlinkTask.WaitFor(raTask)
	chg.AddTask(unlinkTask)

	rmpTask := s.state.NewTask("failure-task", "")
	rmpTask.WaitFor(unlinkTask)
	chg.AddTask(rmpTask)

	// Run the tasks we created
	s.state.Unlock()
	for i := 0; i < 5; i++ {
		s.se.Ensure()
		s.se.Wait()
	}
	s.state.Lock()

	// And observe the results.
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "snapd", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.Active, Equals, true)
	c.Check(snapst.Sequence.Revisions, HasLen, 1)
	c.Check(snapst.Current, Equals, snap.R(20))
	c.Check(raTask.Status(), Equals, state.UndoneStatus)
	c.Check(unlinkTask.Status(), Equals, state.UndoneStatus)
	c.Check(rmpTask.Status(), Equals, state.ErrorStatus)
	// backend was called to unlink the snap
	expected := fakeOps{
		{
			op:   "remove-snap-aliases",
			name: "snapd",
		},
		{
			op:          "run-inhibit-snap-for-unlink",
			name:        "snapd",
			inhibitHint: "refresh",
		},
		{
			op: "update-aliases",
		},
	}
	c.Check(s.fakeBackend.ops, DeepEquals, expected)

	// no restarts must have been requested by 'unlink-current-snap'
	c.Check(s.restartRequested, HasLen, 0)
	c.Assert(unlinkTask.Log(), HasLen, 0)
}

func (s *linkSnapSuite) TestDoUnlinkSnapdUnlinks(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	si := &snap.SideInfo{
		RealName: "snapd",
		Revision: snap.R(20),
	}
	snapstate.Set(s.state, "snapd", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
		Current:  si.Revision,
		Active:   true,
	})

	task := s.state.NewTask("unlink-snap", "")
	task.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Channel:  "beta",
	})
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(task)

	// Run the task we created
	s.state.Unlock()
	s.se.Ensure()
	s.se.Wait()
	s.state.Lock()

	// And observe the results.
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "snapd", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.Active, Equals, false)
	c.Check(snapst.Sequence.Revisions, HasLen, 1)
	c.Check(snapst.Current, Equals, snap.R(20))
	c.Check(task.Status(), Equals, state.DoneStatus)
	// backend was called to unlink the snap
	expected := fakeOps{{
		op:   "unlink-snap",
		path: filepath.Join(dirs.SnapMountDir, "snapd/20"),
	}}
	c.Check(s.fakeBackend.ops, DeepEquals, expected)
}

func (s *linkSnapSuite) TestDoUnlinkCurrentSnapRelinksOnFailure(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// With a snap "foo" at revision 42
	si := &snap.SideInfo{RealName: "foo", Revision: snap.R(42)}
	snapstate.Set(s.state, "foo", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
		Current:  si.Revision,
		Active:   true,
	})

	// With an app belonging to the snap that is apparently running.
	snapstate.MockSnapReadInfo(func(name string, si *snap.SideInfo) (*snap.Info, error) {
		c.Assert(name, Equals, "foo")
		info := &snap.Info{SuggestedName: name, SideInfo: *si, SnapType: snap.TypeApp}
		info.Apps = map[string]*snap.AppInfo{
			"app": {Snap: info, Name: "app"},
		}
		return info, nil
	})
	restore := snapstate.MockPidsOfSnap(func(instanceName string) (map[string][]int, error) {
		c.Assert(instanceName, Equals, "foo")
		return map[string][]int{"snap.foo.app": {1234}}, nil
	})
	defer restore()

	// We can unlink the current revision of that snap, by setting IgnoreRunning flag.
	task := s.state.NewTask("unlink-current-snap", "")
	task.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Flags:    snapstate.Flags{IgnoreRunning: true},
		Type:     "app",
	})
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(task)

	// Inject an error in unlink
	s.fakeBackend.maybeInjectErr = func(op *fakeOp) error {
		if op.op == "unlink-snap" {
			return fmt.Errorf("oh noes something failed during unlink")
		}
		return nil
	}

	// Run the task we created
	s.state.Unlock()
	s.se.Ensure()
	s.se.Wait()
	s.state.Lock()

	// And observe the results.
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "foo", &snapst)
	c.Assert(err, IsNil)

	// We should still see Rev 42 active
	c.Check(snapst.Active, Equals, true)
	c.Check(snapst.Sequence.Revisions, HasLen, 1)
	c.Check(snapst.Current, Equals, snap.R(42))
	c.Check(task.Status(), Equals, state.ErrorStatus)
	expected := fakeOps{
		{
			op:   "unlink-snap",
			path: filepath.Join(dirs.SnapMountDir, "foo/42"),
		},
		// We should see link-snap restoring the snap again as unlink-snap fails
		{
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, "foo/42"),
		},
	}
	c.Check(s.fakeBackend.ops, DeepEquals, expected)
}

func (s *linkSnapSuite) TestDoLinkSnapWithVitalityScore(c *C) {
	s.state.Lock()
	defer s.state.Unlock()
	// a hook might have set some config
	cfg := json.RawMessage(`{"resilience":{"vitality-hint":"bar,foo,baz"}}`)
	err := config.SetSnapConfig(s.state, "core", &cfg)
	c.Assert(err, IsNil)

	si := &snap.SideInfo{
		RealName: "foo",
		Revision: snap.R(33),
	}
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
	})
	t.Set("set-next-boot", true)
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(t)

	s.state.Unlock()

	for i := 0; i < 6; i++ {
		s.se.Ensure()
		s.se.Wait()
	}

	s.state.Lock()
	expected := fakeOps{
		{
			op:    "candidate",
			sinfo: *si,
		},
		{
			op:           "link-snap",
			path:         filepath.Join(dirs.SnapMountDir, "foo/33"),
			vitalityRank: 2,
		},
		{
			op: "maybe-set-next-boot",
		},
	}
	c.Check(s.fakeBackend.ops, DeepEquals, expected)
}

func (s *linkSnapSuite) TestDoLinkSnapTryToCleanupOnError(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	lp := &testLinkParticipant{}
	restore := snapstate.MockLinkSnapParticipants([]snapstate.LinkSnapParticipant{lp, snapstate.LinkSnapParticipantFunc(ifacestate.OnSnapLinkageChanged)})
	defer restore()

	si := &snap.SideInfo{
		RealName: "foo",
		Revision: snap.R(35),
	}
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Channel:  "beta",
	})

	s.fakeBackend.linkSnapFailTrigger = filepath.Join(dirs.SnapMountDir, "foo/35")
	s.state.NewChange("sample", "...").AddTask(t)
	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()

	// state as expected
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "foo", &snapst)
	c.Assert(err, testutil.ErrorIs, state.ErrNoState)

	// tried to cleanup
	expected := fakeOps{
		{
			op:    "candidate",
			sinfo: *si,
		},
		{
			op:   "link-snap.failed",
			path: filepath.Join(dirs.SnapMountDir, "foo/35"),
		},
		{
			op:   "unlink-snap",
			path: filepath.Join(dirs.SnapMountDir, "foo/35"),

			unlinkFirstInstallUndo: true,
		},
	}

	// start with an easier-to-read error if this fails:
	c.Check(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Check(s.fakeBackend.ops, DeepEquals, expected)

	// link snap participant was invoked
	c.Check(lp.instanceNames, DeepEquals, []string{"foo"})
}

func (s *linkSnapSuite) TestDoLinkSnapSuccessCoreRestarts(c *C) {
	restore := release.MockOnClassic(true)
	defer restore()

	s.state.Lock()
	si := &snap.SideInfo{
		RealName: "core",
		Revision: snap.R(33),
	}
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
	})
	s.state.NewChange("sample", "...").AddTask(t)

	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()
	defer s.state.Unlock()

	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "core", &snapst)
	c.Assert(err, IsNil)

	typ, err := snapst.Type()
	c.Check(err, IsNil)
	c.Check(typ, Equals, snap.TypeOS)

	c.Check(t.Status(), Equals, state.DoneStatus)
	c.Check(s.restartRequested, DeepEquals, []restart.RestartType{restart.RestartDaemon})
	c.Check(t.Log(), HasLen, 1)
	c.Check(t.Log()[0], Matches, `.*INFO Requested daemon restart\.`)
}

func (s *linkSnapSuite) TestDoLinkSnapSuccessSnapdRestartsOnCoreWithBase(c *C) {
	restore := release.MockOnClassic(false)
	defer restore()

	r := snapstatetest.MockDeviceModel(ModelWithBase("core18"))
	defer r()

	s.state.Lock()
	si := &snap.SideInfo{
		RealName: "snapd",
		SnapID:   "snapd-snap-id",
		Revision: snap.R(22),
	}
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Type:     snap.TypeSnapd,
	})
	s.state.NewChange("sample", "...").AddTask(t)

	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()
	defer s.state.Unlock()

	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "snapd", &snapst)
	c.Assert(err, IsNil)

	typ, err := snapst.Type()
	c.Check(err, IsNil)
	c.Check(typ, Equals, snap.TypeSnapd)

	c.Check(t.Status(), Equals, state.DoneStatus)
	c.Check(s.restartRequested, DeepEquals, []restart.RestartType{restart.RestartDaemon})
	c.Check(t.Log(), HasLen, 1)
	c.Check(t.Log()[0], Matches, `.*INFO Requested daemon restart \(snapd snap\)\.`)
}

func (s *linkSnapSuite) TestDoLinkSnapSuccessRebootForCoreBase(c *C) {
	restore := release.MockOnClassic(false)
	defer restore()

	r := snapstatetest.MockDeviceModel(ModelWithBase("core18"))
	defer r()

	s.fakeBackend.linkSnapMaybeReboot = true

	s.state.Lock()
	defer s.state.Unlock()

	si := &snap.SideInfo{
		RealName: "core18",
		SnapID:   "core18-id",
		Revision: snap.R(22),
	}
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
	})
	t.Set("set-next-boot", true)

	chg := s.state.NewChange("sample", "...")
	chg.AddTask(t)

	s.state.Unlock()
	s.se.Ensure()
	s.se.Wait()
	s.state.Lock()

	// Ensure that a restart has been requested
	restarting, rt := restart.Pending(s.state)
	c.Check(restarting, Equals, true)
	c.Check(rt, Equals, restart.RestartSystem)
	c.Check(t.Status(), Equals, state.WaitStatus)
	c.Check(s.restartRequested, DeepEquals, []restart.RestartType{restart.RestartSystem})
	c.Assert(t.Log(), HasLen, 1)
	c.Check(t.Log()[0], Matches, `.* INFO Task set to wait until a system restart allows to continue`)
}

func (s *linkSnapSuite) TestDoLinkSnapSuccessRebootForKernelClassicWithModes(c *C) {
	restore := release.MockOnClassic(true)
	defer restore()

	r := snapstatetest.MockDeviceModel(MakeModelClassicWithModes("pc", nil))
	defer r()

	snapstate.MockSnapReadInfo(func(name string, si *snap.SideInfo) (*snap.Info, error) {
		c.Assert(name, Equals, "kernel")
		info := &snap.Info{SuggestedName: name, SideInfo: *si, SnapType: snap.TypeKernel}
		return info, nil
	})

	s.fakeBackend.linkSnapMaybeReboot = true
	s.fakeBackend.linkSnapRebootFor = map[string]bool{
		"kernel": true,
	}

	s.state.Lock()
	defer s.state.Unlock()

	si := &snap.SideInfo{
		RealName: "kernel",
		SnapID:   "pclinuxdidididididididididididid",
		Revision: snap.R(22),
	}
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Type:     snap.TypeKernel,
	})
	t.Set("set-next-boot", true)
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(t)

	s.state.Unlock()
	s.se.Ensure()
	s.se.Wait()
	s.state.Lock()

	// Restart must not have been requested, as we're on classic
	restarting, _ := restart.Pending(s.state)
	c.Check(restarting, Equals, false)
	c.Check(t.Status(), Equals, state.WaitStatus)
	c.Check(s.restartRequested, HasLen, 0)
	c.Assert(t.Log(), HasLen, 1)
	c.Check(t.Log()[0], Matches, `.* INFO Task set to wait until a system restart allows to continue`)
}

func (s *linkSnapSuite) TestDoLinkSnapSuccessRebootForCoreBaseSystemRestartImmediate(c *C) {
	restore := release.MockOnClassic(false)
	defer restore()

	r := snapstatetest.MockDeviceModel(ModelWithBase("core18"))
	defer r()

	s.fakeBackend.linkSnapMaybeReboot = true

	s.state.Lock()
	defer s.state.Unlock()

	// we need to init the boot-id
	//err := s.state.VerifyReboot("some-boot-id")
	//c.Assert(err, IsNil)

	si := &snap.SideInfo{
		RealName: "core18",
		SnapID:   "core18-id",
		Revision: snap.R(22),
	}
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
	})
	t.Set("set-next-boot", true)
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(t)
	chg.Set("system-restart-immediate", true)

	s.state.Unlock()
	s.se.Ensure()
	s.se.Wait()
	s.state.Lock()

	// Ensure the restart is requested as RestartSystemNow
	restarting, rt := restart.Pending(s.state)
	c.Check(restarting, Equals, true)
	c.Check(rt, Equals, restart.RestartSystemNow)
	c.Check(t.Status(), Equals, state.WaitStatus)
	c.Check(s.restartRequested, DeepEquals, []restart.RestartType{restart.RestartSystemNow})
	c.Assert(t.Log(), HasLen, 1)
	c.Check(t.Log()[0], Matches, `.* INFO Task set to wait until a system restart allows to continue`)
}

func (s *linkSnapSuite) TestDoLinkSnapSuccessSnapdRestartsOnClassic(c *C) {
	restore := release.MockOnClassic(true)
	defer restore()

	s.state.Lock()
	si := &snap.SideInfo{
		RealName: "snapd",
		SnapID:   "snapd-snap-id",
		Revision: snap.R(22),
	}
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Type:     snap.TypeSnapd,
	})
	s.state.NewChange("sample", "...").AddTask(t)

	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()
	defer s.state.Unlock()

	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "snapd", &snapst)
	c.Assert(err, IsNil)

	typ, err := snapst.Type()
	c.Check(err, IsNil)
	c.Check(typ, Equals, snap.TypeSnapd)

	c.Check(t.Status(), Equals, state.DoneStatus)
	c.Check(s.restartRequested, DeepEquals, []restart.RestartType{restart.RestartDaemon})
	c.Check(t.Log(), HasLen, 1)
}

func (s *linkSnapSuite) TestDoLinkSnapSuccessGadgetDoesNotRequestReboot(c *C) {
	restore := release.MockOnClassic(false)
	defer restore()

	s.state.Lock()
	si := &snap.SideInfo{
		RealName: "pc",
		SnapID:   "pc-snap-id",
		Revision: snap.R(1),
	}
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Type:     snap.TypeGadget,
	})
	s.state.NewChange("sample", "...").AddTask(t)

	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()
	defer s.state.Unlock()

	// "gadget-restart-required" was not set, so we expect it to run
	// to completion and no reboots
	c.Check(t.Status(), Equals, state.DoneStatus)
	c.Check(s.restartRequested, HasLen, 0)
	c.Check(t.Log(), HasLen, 0)
}

func (s *linkSnapSuite) TestDoLinkSnapSuccessGadgetDoesRequestsRestart(c *C) {
	restore := release.MockOnClassic(false)
	defer restore()

	s.state.Lock()
	si := &snap.SideInfo{
		RealName: "pc",
		SnapID:   "pc-snap-id",
		Revision: snap.R(1),
	}
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Type:     snap.TypeGadget,
	})
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(t)

	// set "gadget-restart-required"
	chg.Set("gadget-restart-required", true)

	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()
	defer s.state.Unlock()

	// Change enters wait-status, and a reboot has been requested
	c.Check(t.Status(), Equals, state.WaitStatus)
	c.Check(s.restartRequested, DeepEquals, []restart.RestartType{restart.RestartSystem})
	c.Check(t.Log(), HasLen, 1)
}

func (s *linkSnapSuite) TestDoLinkSnapFailGadgetDoesRequestsRestart(c *C) {
	restore := release.MockOnClassic(false)
	defer restore()

	s.state.Lock()
	si := &snap.SideInfo{
		RealName: "pc",
		SnapID:   "pc-snap-id",
		Revision: snap.R(1),
	}
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Type:     snap.TypeGadget,
	})
	t.Set("set-next-boot", true)
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(t)

	// Force failure in a contrieved way by setting
	// "gadget-restart-required" to a string (we want to make sure that we
	// unlink on an error after setting next boot)
	chg.Set("gadget-restart-required", "not-bool")

	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()
	defer s.state.Unlock()

	expected := fakeOps{
		{
			op:    "candidate",
			sinfo: *si,
		},
		{
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, "pc/1"),
		},
		{
			op: "maybe-set-next-boot",
		},
		{
			op:   "unlink-snap",
			path: filepath.Join(dirs.SnapMountDir, "pc/1"),

			unlinkFirstInstallUndo: true,
		},
	}
	c.Check(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Check(s.fakeBackend.ops, DeepEquals, expected)

	// Error, no reboot has been requested
	c.Check(t.Status(), Equals, state.ErrorStatus)
	c.Check(s.restartRequested, HasLen, 0)
	c.Check(t.Log(), HasLen, 2)
}

func (s *linkSnapSuite) TestDoLinkSnapSuccessCoreAndSnapdNoCoreRestart(c *C) {
	restore := release.MockOnClassic(true)
	defer restore()

	s.state.Lock()
	siSnapd := &snap.SideInfo{
		RealName: "snapd",
		Revision: snap.R(64),
	}
	snapstate.Set(s.state, "snapd", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{siSnapd}),
		Current:  siSnapd.Revision,
		Active:   true,
		SnapType: "snapd",
	})

	si := &snap.SideInfo{
		RealName: "core",
		Revision: snap.R(33),
	}
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
	})
	s.state.NewChange("sample", "...").AddTask(t)

	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()
	defer s.state.Unlock()

	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "core", &snapst)
	c.Assert(err, IsNil)

	typ, err := snapst.Type()
	c.Check(err, IsNil)
	c.Check(typ, Equals, snap.TypeOS)

	c.Check(t.Status(), Equals, state.DoneStatus)
	c.Check(s.restartRequested, IsNil)
	c.Check(t.Log(), HasLen, 0)
}

func (s *linkSnapSuite) TestDoLinkSnapdSnapCleanupOnErrorFirstInstall(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	lp := &testLinkParticipant{}
	restore := snapstate.MockLinkSnapParticipants([]snapstate.LinkSnapParticipant{lp, snapstate.LinkSnapParticipantFunc(ifacestate.OnSnapLinkageChanged)})
	defer restore()

	si := &snap.SideInfo{
		RealName: "snapd",
		SnapID:   "snapd-snap-id",
		Revision: snap.R(22),
	}
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Channel:  "beta",
	})

	s.fakeBackend.linkSnapFailTrigger = filepath.Join(dirs.SnapMountDir, "snapd/22")
	s.state.NewChange("sample", "...").AddTask(t)
	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()

	// state as expected
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "foo", &snapst)
	c.Assert(err, testutil.ErrorIs, state.ErrNoState)

	// tried to cleanup
	expected := fakeOps{
		{
			op:    "candidate",
			sinfo: *si,
		},
		{
			op:   "link-snap.failed",
			path: filepath.Join(dirs.SnapMountDir, "snapd/22"),
		},
		{
			op:   "unlink-snap",
			path: filepath.Join(dirs.SnapMountDir, "snapd/22"),

			unlinkFirstInstallUndo: true,
		},
	}

	// start with an easier-to-read error if this fails:
	c.Check(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Check(s.fakeBackend.ops, DeepEquals, expected)

	// link snap participant was invoked
	c.Check(lp.instanceNames, DeepEquals, []string{"snapd"})
}

func (s *linkSnapSuite) TestDoLinkSnapdSnapCleanupOnErrorNthInstall(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	lp := &testLinkParticipant{}
	restore := snapstate.MockLinkSnapParticipants([]snapstate.LinkSnapParticipant{lp, snapstate.LinkSnapParticipantFunc(ifacestate.OnSnapLinkageChanged)})
	defer restore()

	si := &snap.SideInfo{
		RealName: "snapd",
		SnapID:   "snapd-snap-id",
		Revision: snap.R(22),
	}
	siOld := *si
	siOld.Revision = snap.R(20)
	snapstate.Set(s.state, "snapd", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{&siOld}),
		Current:  siOld.Revision,
		Active:   true,
		SnapType: "snapd",
	})
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Channel:  "beta",
	})

	s.fakeBackend.linkSnapFailTrigger = filepath.Join(dirs.SnapMountDir, "snapd/22")
	s.state.NewChange("sample", "...").AddTask(t)
	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()

	// state as expected
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "foo", &snapst)
	c.Assert(err, testutil.ErrorIs, state.ErrNoState)

	// tried to cleanup
	expected := fakeOps{
		{
			op:    "candidate",
			sinfo: *si,
		},
		{
			op:   "link-snap.failed",
			path: filepath.Join(dirs.SnapMountDir, "snapd/22"),
		},
		{
			// we link the old revision
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, "snapd/20"),

			unlinkFirstInstallUndo: false,
		},
	}

	// start with an easier-to-read error if this fails:
	c.Check(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Check(s.fakeBackend.ops, DeepEquals, expected)

	// link snap participant was invoked
	c.Check(lp.instanceNames, DeepEquals, []string{"snapd"})
}

func (s *linkSnapSuite) TestDoLinkSnapdDiscardsNsOnDowngrade(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	lp := &testLinkParticipant{}
	restore := snapstate.MockLinkSnapParticipants([]snapstate.LinkSnapParticipant{lp, snapstate.LinkSnapParticipantFunc(ifacestate.OnSnapLinkageChanged)})
	defer restore()

	// pretend we have an installed snapd
	snapstate.MockSnapReadInfo(func(name string, si *snap.SideInfo) (*snap.Info, error) {
		c.Check(name, Equals, "snapd")
		info := &snap.Info{Version: "2.56", SideInfo: *si, SnapType: snap.TypeSnapd}
		return info, nil
	})
	siSnapd := &snap.SideInfo{
		RealName: "snapd",
		SnapID:   "snapd-snap-id",
		Revision: snap.R(42),
	}
	// Create a downgrade
	si := &snap.SideInfo{
		RealName: "snapd",
		SnapID:   "snapd-snap-id",
		Revision: snap.R(41),
	}
	snapstate.Set(s.state, "snapd", &snapstate.SnapState{
		Active:          true,
		Sequence:        snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{siSnapd, si}),
		Current:         siSnapd.Revision,
		TrackingChannel: "latest/stable",
		SnapType:        "snapd",
	})
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Channel:  "beta",
	})
	t.Set("set-next-boot", true)

	s.state.NewChange("sample", "...").AddTask(t)
	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()

	// tried to cleanup
	expected := fakeOps{
		{
			op:    "candidate",
			sinfo: *si,
		},
		{
			op:   "discard-namespace",
			name: "snapd",
		},
		{
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, "snapd/41"),
		},
		{
			op: "maybe-set-next-boot",
		},
	}

	// start with an easier-to-read error if this fails:
	c.Check(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Check(s.fakeBackend.ops, DeepEquals, expected)

	// link snap participant was invoked
	c.Check(lp.instanceNames, DeepEquals, []string{"snapd"})
}

func (s *linkSnapSuite) TestDoLinkSnapdRemovesAppArmorProfilesOnSnapdDowngrade(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	lp := &testLinkParticipant{}
	restore := snapstate.MockLinkSnapParticipants([]snapstate.LinkSnapParticipant{lp, snapstate.LinkSnapParticipantFunc(ifacestate.OnSnapLinkageChanged)})
	defer restore()

	// pretend we have an installed snapd with a vendored apparmor that has
	// a version greater than the one we are going to downgrade to
	restore = snapdtool.MockVersion("2.58")
	defer restore()
	restore = apparmor.MockFeatures([]string{}, nil, []string{"snapd-internal"}, nil)
	defer restore()
	snapstate.MockSnapReadInfo(func(name string, si *snap.SideInfo) (*snap.Info, error) {
		c.Check(name, Equals, "snapd")
		info := &snap.Info{Version: "2.56", SideInfo: *si, SnapType: snap.TypeSnapd}
		return info, nil
	})

	siSnapd := &snap.SideInfo{
		RealName: "snapd",
		SnapID:   "snapd-snap-id",
		Revision: snap.R(42),
	}
	// Create a downgrade
	si := &snap.SideInfo{
		RealName: "snapd",
		SnapID:   "snapd-snap-id",
		Revision: snap.R(41),
	}
	snapstate.Set(s.state, "snapd", &snapstate.SnapState{
		Active:          true,
		Sequence:        snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{siSnapd, si}),
		Current:         siSnapd.Revision,
		TrackingChannel: "latest/stable",
		SnapType:        "snapd",
	})
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Channel:  "beta",
	})
	t.Set("set-next-boot", true)

	// set seeded so that AppArmor profile cleanup should occur - however
	// since we now appear to be seeded this would trigger the mount units
	// to be updated etc which would call into systemctl - so turn that off
	// too
	s.state.Set("seeded", true)
	s.AddCleanup(snapstate.MockEnsuredMountsUpdated(s.snapmgr, true))
	s.state.NewChange("sample", "...").AddTask(t)
	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()

	// tried to cleanup
	expected := fakeOps{
		{
			op:    "candidate",
			sinfo: *si,
		},
		{
			op:   "discard-namespace",
			name: "snapd",
		},
		{
			op: "remove-apparmor-profiles",
		},
		{
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, "snapd/41"),
		},
		{
			op: "maybe-set-next-boot",
		},
	}

	// start with an easier-to-read error if this fails:
	c.Check(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Check(s.fakeBackend.ops, DeepEquals, expected)

	// link snap participant was invoked
	c.Check(lp.instanceNames, DeepEquals, []string{"snapd"})
}

func (s *linkSnapSuite) TestDoUndoLinkSnapSequenceDidNotHaveCandidate(c *C) {
	s.state.Lock()
	defer s.state.Unlock()
	si1 := &snap.SideInfo{
		RealName: "foo",
		Revision: snap.R(1),
	}
	si2 := &snap.SideInfo{
		RealName: "foo",
		Revision: snap.R(2),
	}
	snapstate.Set(s.state, "foo", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si1}),
		Current:  si1.Revision,
	})
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si2,
		Channel:  "beta",
	})
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(t)

	terr := s.state.NewTask("error-trigger", "provoking total undo")
	terr.WaitFor(t)
	chg.AddTask(terr)

	s.state.Unlock()

	for i := 0; i < 6; i++ {
		s.se.Ensure()
		s.se.Wait()
	}

	s.state.Lock()
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "foo", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.Active, Equals, false)
	c.Check(snapst.Sequence.Revisions, HasLen, 1)
	c.Check(snapst.Current, Equals, snap.R(1))
	c.Check(t.Status(), Equals, state.UndoneStatus)
}

func (s *linkSnapSuite) TestDoUndoLinkSnapSequenceHadCandidate(c *C) {
	s.state.Lock()
	defer s.state.Unlock()
	si1 := &snap.SideInfo{
		RealName: "foo",
		Revision: snap.R(1),
	}
	si2 := &snap.SideInfo{
		RealName: "foo",
		Revision: snap.R(2),
	}
	snapstate.Set(s.state, "foo", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si1, si2}),
		Current:  si2.Revision,
	})
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si1,
		Channel:  "beta",
	})
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(t)

	terr := s.state.NewTask("error-trigger", "provoking total undo")
	terr.WaitFor(t)
	chg.AddTask(terr)

	s.state.Unlock()

	for i := 0; i < 6; i++ {
		s.se.Ensure()
		s.se.Wait()
	}

	s.state.Lock()
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "foo", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.Active, Equals, false)
	c.Check(snapst.Sequence.Revisions, HasLen, 2)
	c.Check(snapst.Current, Equals, snap.R(2))
	c.Check(t.Status(), Equals, state.UndoneStatus)
}

func (s *linkSnapSuite) TestDoUndoUnlinkCurrentSnapCore(c *C) {
	restore := release.MockOnClassic(true)
	defer restore()

	linkChangeCount := 0
	lp := &testLinkParticipant{
		linkageChanged: func(st *state.State, snapsup *snapstate.SnapSetup) error {
			var snapst snapstate.SnapState
			err := snapstate.Get(st, snapsup.InstanceName(), &snapst)
			linkChangeCount++
			switch linkChangeCount {
			case 1:
				// Initially the snap gets unlinked.
				c.Check(err, IsNil)
				c.Check(snapst.Active, Equals, false)
				c.Check(snapst.PendingSecurity, NotNil)
			case 2:
				// Then the undo handler re-links it.
				c.Check(err, IsNil)
				c.Check(snapst.Active, Equals, true)
				c.Check(snapst.PendingSecurity, IsNil)
			}
			return nil
		},
	}
	restore = snapstate.MockLinkSnapParticipants([]snapstate.LinkSnapParticipant{snapstate.LinkSnapParticipantFunc(ifacestate.OnSnapLinkageChanged), lp})
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()
	si1 := &snap.SideInfo{
		RealName: "core",
		Revision: snap.R(1),
	}
	si2 := &snap.SideInfo{
		RealName: "core",
		Revision: snap.R(2),
	}
	snapstate.Set(s.state, "core", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si1}),
		Current:  si1.Revision,
		Active:   true,
		SnapType: "os",
	})
	t := s.state.NewTask("unlink-current-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si2,
	})
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(t)

	terr := s.state.NewTask("error-trigger", "provoking total undo")
	terr.WaitFor(t)
	chg.AddTask(terr)

	s.state.Unlock()

	for i := 0; i < 3; i++ {
		s.se.Ensure()
		s.se.Wait()
	}

	s.state.Lock()
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "core", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.Active, Equals, true)
	c.Check(snapst.Sequence.Revisions, HasLen, 1)
	c.Check(snapst.Current, Equals, snap.R(1))
	c.Check(t.Status(), Equals, state.UndoneStatus)

	c.Check(s.restartRequested, DeepEquals, []restart.RestartType{restart.RestartDaemon})
	c.Check(lp.instanceNames, DeepEquals, []string{"core", "core"})
}

func (s *linkSnapSuite) TestDoUndoUnlinkCurrentSnapCoreBase(c *C) {
	restore := release.MockOnClassic(false)
	defer restore()

	r := snapstatetest.MockDeviceModel(ModelWithBase("core18"))
	defer r()

	s.fakeBackend.linkSnapMaybeReboot = true

	s.state.Lock()
	defer s.state.Unlock()

	si1 := &snap.SideInfo{
		RealName: "core18",
		Revision: snap.R(1),
	}
	si2 := &snap.SideInfo{
		RealName: "core18",
		Revision: snap.R(2),
	}
	snapstate.Set(s.state, "core18", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si1}),
		Current:  si1.Revision,
		Active:   true,
		SnapType: "base",
	})
	t := s.state.NewTask("unlink-current-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si2,
	})
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(t)

	terr := s.state.NewTask("error-trigger", "provoking total undo")
	terr.WaitFor(t)
	chg.AddTask(terr)

	s.state.Unlock()
	for i := 0; i < 3; i++ {
		s.se.Ensure()
		s.se.Wait()
	}
	s.state.Lock()

	// simulate a restart
	restart.MockPending(s.state, restart.RestartUnset)
	restart.MockAfterRestartForChange(chg)

	s.state.Unlock()
	s.se.Ensure()
	s.se.Wait()
	s.state.Lock()

	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "core18", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.Active, Equals, true)
	c.Check(snapst.Sequence.Revisions, HasLen, 1)
	c.Check(snapst.Current, Equals, snap.R(1))
	c.Check(t.Status(), Equals, state.UndoneStatus)

	c.Check(s.restartRequested, DeepEquals, []restart.RestartType{restart.RestartSystem})
}

func (s *linkSnapSuite) TestDoUndoLinkSnapCoreClassic(c *C) {
	restore := release.MockOnClassic(true)
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	// no previous core snap and an error on link, in this
	// case we need to restart on classic back into the distro
	// package version
	si1 := &snap.SideInfo{
		RealName: "core",
		Revision: snap.R(1),
	}
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si1,
	})
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(t)

	terr := s.state.NewTask("error-trigger", "provoking total undo")
	terr.WaitFor(t)
	chg.AddTask(terr)

	s.state.Unlock()

	for i := 0; i < 3; i++ {
		s.se.Ensure()
		s.se.Wait()
	}

	s.state.Lock()
	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "core", &snapst)
	c.Assert(err, testutil.ErrorIs, state.ErrNoState)
	c.Check(t.Status(), Equals, state.UndoneStatus)

	c.Check(s.restartRequested, DeepEquals, []restart.RestartType{restart.RestartDaemon, restart.RestartDaemon})

}

func (s *linkSnapSuite) TestLinkSnapInjectsAutoConnectIfMissing(c *C) {
	si1 := &snap.SideInfo{
		RealName: "snap1",
		Revision: snap.R(1),
	}
	sup1 := &snapstate.SnapSetup{SideInfo: si1}
	si2 := &snap.SideInfo{
		RealName: "snap2",
		Revision: snap.R(1),
	}
	sup2 := &snapstate.SnapSetup{SideInfo: si2}

	s.state.Lock()
	defer s.state.Unlock()

	task0 := s.state.NewTask("setup-profiles", "")
	task1 := s.state.NewTask("link-snap", "")
	task1.WaitFor(task0)
	task0.Set("snap-setup", sup1)
	task1.Set("snap-setup", sup1)

	task2 := s.state.NewTask("setup-profiles", "")
	task3 := s.state.NewTask("link-snap", "")
	task2.WaitFor(task1)
	task3.WaitFor(task2)
	task2.Set("snap-setup", sup2)
	task3.Set("snap-setup", sup2)

	chg := s.state.NewChange("test", "")
	chg.AddTask(task0)
	chg.AddTask(task1)
	chg.AddTask(task2)
	chg.AddTask(task3)

	s.state.Unlock()

	for i := 0; i < 10; i++ {
		s.se.Ensure()
		s.se.Wait()
	}

	s.state.Lock()

	// ensure all our tasks ran
	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.Tasks(), HasLen, 6)

	// validity checks
	t := chg.Tasks()[1]
	c.Assert(t.Kind(), Equals, "link-snap")
	t = chg.Tasks()[3]
	c.Assert(t.Kind(), Equals, "link-snap")

	// check that auto-connect tasks were added and have snap-setup
	var autoconnectSup snapstate.SnapSetup
	t = chg.Tasks()[4]
	c.Assert(t.Kind(), Equals, "auto-connect")
	c.Assert(t.Get("snap-setup", &autoconnectSup), IsNil)
	c.Assert(autoconnectSup.InstanceName(), Equals, "snap1")

	t = chg.Tasks()[5]
	c.Assert(t.Kind(), Equals, "auto-connect")
	c.Assert(t.Get("snap-setup", &autoconnectSup), IsNil)
	c.Assert(autoconnectSup.InstanceName(), Equals, "snap2")
}

func (s *linkSnapSuite) TestDoLinkSnapFailureCleansUpAux(c *C) {
	// this is very chummy with the order of LinkSnap
	c.Assert(os.WriteFile(dirs.SnapSeqDir, nil, 0644), IsNil)

	// we start without the auxiliary store info
	c.Check(backend.AuxStoreInfoFilename("foo-id"), testutil.FileAbsent)

	s.state.Lock()
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: &snap.SideInfo{
			RealName: "foo",
			Revision: snap.R(33),
			SnapID:   "foo-id",
		},
		Channel: "beta",
		UserID:  2,
	})
	s.state.NewChange("sample", "...").AddTask(t)

	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()
	defer s.state.Unlock()

	c.Check(t.Status(), Equals, state.ErrorStatus)
	c.Check(s.restartRequested, HasLen, 0)

	// we end without the auxiliary store info
	c.Check(backend.AuxStoreInfoFilename("foo-id"), testutil.FileAbsent)
}

func (s *linkSnapSuite) TestLinkSnapResetsRefreshInhibitedTime(c *C) {
	// When a snap is linked the refresh-inhibited-time is reset to zero
	// to indicate a successful refresh. The old value is stored in task
	// state for task undo logic.
	s.state.Lock()
	defer s.state.Unlock()

	instant := time.Now()

	si := &snap.SideInfo{RealName: "snap", Revision: snap.R(1)}
	sup := &snapstate.SnapSetup{SideInfo: si}
	snapstate.Set(s.state, "snap", &snapstate.SnapState{
		Sequence:             snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
		Current:              si.Revision,
		RefreshInhibitedTime: &instant,
	})

	task := s.state.NewTask("link-snap", "")
	task.Set("snap-setup", sup)
	chg := s.state.NewChange("test", "")
	chg.AddTask(task)

	s.state.Unlock()

	for i := 0; i < 10; i++ {
		s.se.Ensure()
		s.se.Wait()
	}

	s.state.Lock()

	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.Tasks(), HasLen, 1)

	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "snap", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.RefreshInhibitedTime, IsNil)

	var oldTime time.Time
	c.Assert(task.Get("old-refresh-inhibited-time", &oldTime), IsNil)
	c.Check(oldTime.Equal(instant), Equals, true)
}

func (s *linkSnapSuite) TestDoUndoLinkSnapRestoresRefreshInhibitedTime(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	instant := time.Now()

	si := &snap.SideInfo{RealName: "snap", Revision: snap.R(1)}
	sup := &snapstate.SnapSetup{SideInfo: si}
	snapstate.Set(s.state, "snap", &snapstate.SnapState{
		Sequence:             snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
		Current:              si.Revision,
		RefreshInhibitedTime: &instant,
	})

	task := s.state.NewTask("link-snap", "")
	task.Set("snap-setup", sup)
	chg := s.state.NewChange("test", "")
	chg.AddTask(task)

	terr := s.state.NewTask("error-trigger", "provoking total undo")
	terr.WaitFor(task)
	chg.AddTask(terr)

	s.state.Unlock()

	for i := 0; i < 6; i++ {
		s.se.Ensure()
		s.se.Wait()
	}

	s.state.Lock()

	c.Assert(chg.Err(), NotNil)
	c.Assert(chg.Tasks(), HasLen, 2)
	c.Check(task.Status(), Equals, state.UndoneStatus)

	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "snap", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.RefreshInhibitedTime.Equal(instant), Equals, true)
}

func (s *linkSnapSuite) TestLinkSnapSetsLastRefreshTime(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	now, err := time.Parse(time.RFC3339, "2021-07-20T10:00:00Z")
	c.Assert(err, IsNil)
	restoreTimeNow := snapstate.MockTimeNow(func() time.Time {
		return now
	})
	defer restoreTimeNow()

	si := &snap.SideInfo{RealName: "snap", Revision: snap.R(1)}
	sup := &snapstate.SnapSetup{SideInfo: si}
	snapstate.Set(s.state, "snap", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
		Current:  si.Revision,
		// LastRefreshTime not set initially (first install)
	})

	task := s.state.NewTask("link-snap", "")
	task.Set("snap-setup", sup)
	chg := s.state.NewChange("test", "")
	chg.AddTask(task)

	s.state.Unlock()

	for i := 0; i < 10; i++ {
		s.se.Ensure()
		s.se.Wait()
	}

	s.state.Lock()

	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.Tasks(), HasLen, 1)

	var snapst snapstate.SnapState
	c.Assert(snapstate.Get(s.state, "snap", &snapst), IsNil)
	c.Assert(snapst.LastRefreshTime, NotNil)
	c.Check(snapst.LastRefreshTime.Equal(now), Equals, true)

	var oldTime *time.Time
	c.Assert(task.Get("old-last-refresh-time", &oldTime), IsNil)
	c.Check(oldTime, IsNil)
}

func (s *linkSnapSuite) TestLinkSnapUpdatesLastRefreshTime(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	now, err := time.Parse(time.RFC3339, "2021-07-20T10:00:00Z")
	c.Assert(err, IsNil)
	restoreTimeNow := snapstate.MockTimeNow(func() time.Time {
		return now
	})
	defer restoreTimeNow()

	lastRefresh, err := time.Parse(time.RFC3339, "2021-02-20T10:00:00Z")
	c.Assert(err, IsNil)
	si := &snap.SideInfo{RealName: "snap", Revision: snap.R(1)}
	sup := &snapstate.SnapSetup{SideInfo: si}
	snapstate.Set(s.state, "snap", &snapstate.SnapState{
		Sequence:        snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
		Current:         si.Revision,
		LastRefreshTime: &lastRefresh,
	})

	task := s.state.NewTask("link-snap", "")
	task.Set("snap-setup", sup)
	chg := s.state.NewChange("test", "")
	chg.AddTask(task)

	s.state.Unlock()

	for i := 0; i < 10; i++ {
		s.se.Ensure()
		s.se.Wait()
	}

	s.state.Lock()

	c.Assert(chg.Err(), IsNil)

	var snapst snapstate.SnapState
	c.Assert(snapstate.Get(s.state, "snap", &snapst), IsNil)
	c.Assert(snapst.LastRefreshTime, NotNil)
	c.Check(snapst.LastRefreshTime.Equal(now), Equals, true)

	var oldTime *time.Time
	c.Assert(task.Get("old-last-refresh-time", &oldTime), IsNil)
	c.Check(oldTime.Equal(lastRefresh), Equals, true)
}

func (s *linkSnapSuite) TestDoUndoLinkSnapRestoresLastRefreshTime(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	lastRefresh, err := time.Parse(time.RFC3339, "2021-06-10T10:00:00Z")
	c.Assert(err, IsNil)

	restoreTimeNow := snapstate.MockTimeNow(func() time.Time {
		now, err := time.Parse(time.RFC3339, "2021-07-20T10:00:00Z")
		c.Assert(err, IsNil)
		return now
	})
	defer restoreTimeNow()

	si := &snap.SideInfo{RealName: "snap", Revision: snap.R(1)}
	sup := &snapstate.SnapSetup{SideInfo: si}
	snapstate.Set(s.state, "snap", &snapstate.SnapState{
		Sequence:        snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
		Current:         si.Revision,
		LastRefreshTime: &lastRefresh,
	})

	task := s.state.NewTask("link-snap", "")
	task.Set("snap-setup", sup)
	chg := s.state.NewChange("test", "")
	chg.AddTask(task)

	terr := s.state.NewTask("error-trigger", "provoking total undo")
	terr.WaitFor(task)
	chg.AddTask(terr)

	s.state.Unlock()
	for i := 0; i < 6; i++ {
		s.se.Ensure()
		s.se.Wait()
	}
	s.state.Lock()

	c.Assert(chg.Err(), NotNil)
	c.Check(task.Status(), Equals, state.UndoneStatus)

	var snapst snapstate.SnapState
	c.Assert(snapstate.Get(s.state, "snap", &snapst), IsNil)
	// the original last-refresh-time has been restored.
	c.Check(snapst.LastRefreshTime.Equal(lastRefresh), Equals, true)
}

func (s *linkSnapSuite) TestUndoLinkSnapdFirstInstall(c *C) {
	restore := release.MockOnClassic(false)
	defer restore()

	s.state.Lock()
	si := &snap.SideInfo{
		RealName: "snapd",
		SnapID:   "snapd-snap-id",
		Revision: snap.R(22),
	}
	chg := s.state.NewChange("sample", "...")
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Type:     snap.TypeSnapd,
	})
	t.Set("set-next-boot", true)
	chg.AddTask(t)
	terr := s.state.NewTask("error-trigger", "provoking total undo")
	terr.WaitFor(t)
	chg.AddTask(terr)

	s.state.Unlock()

	for i := 0; i < 6; i++ {
		s.se.Ensure()
		s.se.Wait()
	}

	s.state.Lock()
	defer s.state.Unlock()

	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "snapd", &snapst)
	c.Assert(err, testutil.ErrorIs, state.ErrNoState)
	c.Check(t.Status(), Equals, state.UndoneStatus)

	expected := fakeOps{
		{
			op:    "candidate",
			sinfo: *si,
		},
		{
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, "snapd/22"),
		},
		{
			op: "maybe-set-next-boot",
		},
		{
			op:   "discard-namespace",
			name: "snapd",
		},
		{
			op:   "unlink-snap",
			path: filepath.Join(dirs.SnapMountDir, "snapd/22"),

			unlinkFirstInstallUndo: true,
		},
	}

	// start with an easier-to-read error if this fails:
	c.Check(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Check(s.fakeBackend.ops, DeepEquals, expected)

	// 2 restarts, one from link snap, another one from undo
	c.Check(s.restartRequested, DeepEquals, []restart.RestartType{restart.RestartDaemon, restart.RestartDaemon})
	c.Check(t.Log(), HasLen, 3)
	c.Check(t.Log()[0], Matches, `.*INFO Requested daemon restart \(snapd snap\)\.`)
	c.Check(t.Log()[2], Matches, `.*INFO Requested daemon restart \(snapd snap\)\.`)

}

func (s *linkSnapSuite) TestUndoLinkSnapdNthInstall(c *C) {
	restore := release.MockOnClassic(false)
	defer restore()

	s.state.Lock()
	si := &snap.SideInfo{
		RealName: "snapd",
		SnapID:   "snapd-snap-id",
		Revision: snap.R(22),
	}
	siOld := *si
	siOld.Revision = snap.R(20)
	snapstate.Set(s.state, "snapd", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{&siOld}),
		Current:  siOld.Revision,
		Active:   true,
		SnapType: "snapd",
	})
	chg := s.state.NewChange("sample", "...")
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Type:     snap.TypeSnapd,
	})
	t.Set("set-next-boot", true)
	chg.AddTask(t)
	terr := s.state.NewTask("error-trigger", "provoking total undo")
	terr.WaitFor(t)
	chg.AddTask(terr)

	s.state.Unlock()

	for i := 0; i < 6; i++ {
		s.se.Ensure()
		s.se.Wait()
	}

	s.state.Lock()
	defer s.state.Unlock()

	var snapst snapstate.SnapState
	err := snapstate.Get(s.state, "snapd", &snapst)
	c.Assert(err, IsNil)
	snapst.Current = siOld.Revision
	c.Check(t.Status(), Equals, state.UndoneStatus)

	expected := fakeOps{
		{
			op:    "candidate",
			sinfo: *si,
		},
		{
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, "snapd/22"),
		},
		{
			op: "maybe-set-next-boot",
		},
		{
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, "snapd/20"),
		},
	}

	// start with an easier-to-read error if this fails:
	c.Check(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Check(s.fakeBackend.ops, DeepEquals, expected)

	// 1 restart from link snap
	// 1 restart from undo of 'setup-profiles'
	c.Check(s.restartRequested, DeepEquals, []restart.RestartType{restart.RestartDaemon, restart.RestartDaemon})
	c.Assert(t.Log(), HasLen, 2)
	c.Check(t.Log()[0], Matches, `.*INFO Requested daemon restart \(snapd snap\)\.`)
	c.Check(t.Log()[1], Matches, `.*INFO Requested daemon restart \(snapd snap\)\.`)

	// verify sequence file
	// and check that the sequence file got updated
	seqContent, err := os.ReadFile(filepath.Join(dirs.SnapSeqDir, "snapd.json"))
	c.Assert(err, IsNil)
	c.Check(string(seqContent), Equals, `{"sequence":[{"name":"snapd","snap-id":"snapd-snap-id","revision":"20"}],"current":"20","migrated-hidden":false,"migrated-exposed-home":false}`)
}

func (s *linkSnapSuite) TestDoUnlinkSnapRefreshAwarenessHardCheckOn(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.refresh-app-awareness", true)
	tr.Commit()

	chg := s.testDoUnlinkSnapRefreshAwareness(c)

	c.Check(chg.Err(), ErrorMatches, `(?ms).*^- some-change-descr \(snap "some-snap" has running apps \(some-app\), pids: 1234\).*`)
}

func (s *linkSnapSuite) TestDoUnlinkSnapRefreshHardCheckOff(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.refresh-app-awareness", false)
	tr.Commit()

	chg := s.testDoUnlinkSnapRefreshAwareness(c)

	c.Check(chg.Err(), IsNil)
}

func (s *linkSnapSuite) testDoUnlinkSnapRefreshAwareness(c *C) *state.Change {
	restore := release.MockOnClassic(true)
	defer restore()

	dirs.SetRootDir(c.MkDir())
	defer dirs.SetRootDir("/")

	snapstate.MockSnapReadInfo(func(name string, si *snap.SideInfo) (*snap.Info, error) {
		info := &snap.Info{SuggestedName: name, SideInfo: *si, SnapType: snap.TypeApp}
		info.Apps = map[string]*snap.AppInfo{
			"some-app": {Snap: info, Name: "some-app"},
		}
		return info, nil
	})
	// mock that "some-snap" has an app and that this app has pids running
	restore = snapstate.MockPidsOfSnap(func(instanceName string) (map[string][]int, error) {
		c.Assert(instanceName, Equals, "some-snap")
		return map[string][]int{
			"snap.some-snap.some-app": {1234},
		}, nil
	})
	defer restore()

	si1 := &snap.SideInfo{
		RealName: "some-snap",
		Revision: snap.R(1),
	}
	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si1}),
		Current:  si1.Revision,
		Active:   true,
	})
	t := s.state.NewTask("unlink-current-snap", "some-change-descr")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si1,
	})
	chg := s.state.NewChange("sample", "...")
	chg.AddTask(t)

	s.state.Unlock()
	defer s.state.Lock()

	for i := 0; i < 3; i++ {
		s.se.Ensure()
		s.se.Wait()
	}

	return chg
}

func (s *linkSnapSuite) setMockKernelRemodelCtx(c *C, oldKernel, newKernel string) {
	newModel := MakeModel(map[string]any{"kernel": newKernel})
	oldModel := MakeModel(map[string]any{"kernel": oldKernel})
	mockRemodelCtx := &snapstatetest.TrivialDeviceContext{
		DeviceModel:    newModel,
		OldDeviceModel: oldModel,
		Remodeling:     true,
	}
	restore := snapstatetest.MockDeviceContext(mockRemodelCtx)
	s.AddCleanup(restore)
}

func (s *linkSnapSuite) TestMaybeUndoRemodelBootChangesUnrelatedAppDoesNothing(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	s.setMockKernelRemodelCtx(c, "kernel", "new-kernel")
	t := s.state.NewTask("link-snap", "...")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: &snap.SideInfo{
			RealName: "some-app",
			Revision: snap.R(1),
		},
	})

	restartRequested, _, err := s.snapmgr.MaybeUndoRemodelBootChanges(t)
	c.Assert(err, IsNil)
	c.Check(restartRequested, Equals, false)
}

func (s *linkSnapSuite) TestMaybeUndoRemodelBootChangesSameKernel(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	s.setMockKernelRemodelCtx(c, "kernel", "kernel")
	t := s.state.NewTask("link-snap", "...")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: &snap.SideInfo{
			RealName: "kernel",
			Revision: snap.R(1),
		},
		Type: "kernel",
	})

	restartRequested, _, err := s.snapmgr.MaybeUndoRemodelBootChanges(t)
	c.Assert(err, IsNil)
	c.Check(restartRequested, Equals, false)
}

func (s *linkSnapSuite) TestMaybeUndoRemodelBootChangesNeedsUndo(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// undoing remodel bootenv changes is only relevant on !classic
	restore := release.MockOnClassic(false)
	defer restore()

	// using "snaptest.MockSnap()" is more convenient here so we avoid
	// the (default) mocking of snapReadInfo()
	restore = snapstate.MockSnapReadInfo(snap.ReadInfo)
	defer restore()

	// we pretend we do a remodel from kernel -> new-kernel
	s.setMockKernelRemodelCtx(c, "kernel", "new-kernel")

	// and we pretend that we booted into the "new-kernel" already
	// and now that needs to be undone
	bloader := boottest.MockUC16Bootenv(bootloadertest.Mock("mock", c.MkDir()))
	bootloader.Force(bloader)
	bloader.SetBootKernel("new-kernel_1.snap")

	// both kernel and new-kernel are instaleld at this point
	si := &snap.SideInfo{RealName: "kernel", Revision: snap.R(1)}
	snapstate.Set(s.state, "kernel", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
		SnapType: "kernel",
		Current:  snap.R(1),
	})
	snaptest.MockSnap(c, "name: kernel\ntype: kernel\nversion: 1.0", si)
	si2 := &snap.SideInfo{RealName: "new-kernel", Revision: snap.R(1)}
	snapstate.Set(s.state, "new-kernel", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si2}),
		SnapType: "kernel",
		Current:  snap.R(1),
	})
	snaptest.MockSnap(c, "name: new-kernel\ntype: kernel\nversion: 1.0", si)

	t := s.state.NewTask("link-snap", "...")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: &snap.SideInfo{
			RealName: "new-kernel",
			Revision: snap.R(1),
			SnapID:   "new-kernel-id",
		},
		Type: "kernel",
	})
	s.state.NewChange("sample", "...").AddTask(t)

	// now we simulate that the new kernel is getting undone
	restartRequested, rebootRequired, err := s.snapmgr.MaybeUndoRemodelBootChanges(t)
	c.Assert(err, IsNil)

	// that will schedule a boot into the previous kernel
	c.Assert(bloader.BootVars, DeepEquals, map[string]string{
		"snap_mode":       boot.DefaultStatus,
		"snap_kernel":     "kernel_1.snap",
		"snap_try_kernel": "",
	})
	c.Check(restartRequested, Equals, true)
	c.Check(rebootRequired, Equals, true)
}

func (s *linkSnapSuite) testDoLinkSnapWithToolingDependency(c *C, classicOrBase string) {
	var model *asserts.Model
	var needsTooling bool
	switch classicOrBase {
	case "classic-system":
		model = ClassicModel()
	case "":
		model = DefaultModel()
	default:
		// the tooling mount is needed on UC18+
		needsTooling = true
		model = ModelWithBase(classicOrBase)
	}
	r := snapstatetest.MockDeviceModel(model)
	defer r()

	s.state.Lock()
	si := &snap.SideInfo{
		RealName: "services-snap",
		SnapID:   "services-snap-id",
		Revision: snap.R(11),
	}
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Type:     snap.TypeApp,
	})
	t.Set("set-next-boot", true)
	s.state.NewChange("sample", "...").AddTask(t)

	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()
	defer s.state.Unlock()

	expected := fakeOps{
		{
			op:    "candidate",
			sinfo: *si,
		},
		{
			op:                  "link-snap",
			path:                filepath.Join(dirs.SnapMountDir, "services-snap/11"),
			requireSnapdTooling: needsTooling,
		},
		{
			op: "maybe-set-next-boot",
		},
	}

	// start with an easier-to-read error if this fails:
	c.Check(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Check(s.fakeBackend.ops, DeepEquals, expected)
}

func (s *linkSnapSuite) TestDoLinkSnapWithToolingClassic(c *C) {
	s.testDoLinkSnapWithToolingDependency(c, "classic-system")
}

func (s *linkSnapSuite) TestDoLinkSnapWithToolingCore(c *C) {
	s.testDoLinkSnapWithToolingDependency(c, "")
}

func (s *linkSnapSuite) TestDoLinkSnapWithToolingCore18(c *C) {
	s.testDoLinkSnapWithToolingDependency(c, "core18")
}

func (s *linkSnapSuite) TestDoLinkSnapWithToolingCore20(c *C) {
	s.testDoLinkSnapWithToolingDependency(c, "core20")
}

func (s *linkSnapSuite) testDoKillSnapApps(c *C, svc bool) {
	s.state.Lock()
	defer s.state.Unlock()

	si := &snap.SideInfo{
		RealName: "some-snap",
		Revision: snap.R(1),
	}
	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
		Current:  si.Revision,
		Active:   true,
	})

	restore := snapstate.MockSnapReadInfo(func(name string, si *snap.SideInfo) (*snap.Info, error) {
		c.Assert(name, Equals, "some-snap")
		info := &snap.Info{SuggestedName: name, SideInfo: *si, SnapType: snap.TypeApp}
		if svc {
			info.Apps = map[string]*snap.AppInfo{
				"svc1": {Snap: info, Name: "svc1", Daemon: "simple"},
			}
		}
		return info, nil
	})
	defer restore()

	task := s.state.NewTask("kill-snap-apps", "")
	task.Set("kill-reason", snap.KillReasonRemove)
	task.Set("snap-setup", &snapstate.SnapSetup{SideInfo: si})
	chg := s.state.NewChange("test", "")
	chg.AddTask(task)

	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()

	c.Assert(chg.Err(), IsNil)

	expected := fakeOps{
		{
			op:         "kill-snap-apps:remove",
			name:       "some-snap",
			snapLocked: true,
		},
	}
	if svc {
		expected = append(expected, fakeOp{
			op:   "stop-snap-services:remove",
			path: filepath.Join(dirs.SnapMountDir, "some-snap/1"),
		})
	}

	c.Assert(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Check(s.fakeBackend.ops, DeepEquals, expected)

	hint, _, err := runinhibit.IsLocked("some-snap", nil)
	c.Assert(err, IsNil)
	c.Check(hint, Equals, runinhibit.HintInhibitedForRemove)

	// Snap lock is unlocked after kill-snap-apps returns
	testLock, err := snaplock.OpenLock("some-snap")
	c.Assert(err, IsNil)
	c.Check(testLock.TryLock(), IsNil)
	testLock.Close()
}

func (s *linkSnapSuite) TestDoKillSnapApps(c *C) {
	const svc = false
	s.testDoKillSnapApps(c, svc)
}

func (s *linkSnapSuite) TestDoKillSnapAppsWithServices(c *C) {
	const svc = true
	s.testDoKillSnapApps(c, svc)
}

func (s *linkSnapSuite) TestDoKillSnapAppsUnlocksOnError(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	si := &snap.SideInfo{
		RealName: "some-snap",
		Revision: snap.R(1),
	}
	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
		Current:  si.Revision,
		Active:   true,
	})

	snapstate.MockSnapReadInfo(func(name string, si *snap.SideInfo) (*snap.Info, error) {
		return nil, fmt.Errorf("boom!")
	})

	task := s.state.NewTask("kill-snap-apps", "")
	task.Set("kill-reason", snap.KillReasonRemove)
	task.Set("snap-setup", &snapstate.SnapSetup{SideInfo: si})
	chg := s.state.NewChange("test", "")
	chg.AddTask(task)

	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()

	c.Assert(task.Status(), Equals, state.ErrorStatus)

	hint, _, err := runinhibit.IsLocked("some-snap", nil)
	c.Assert(err, IsNil)
	// On error hint inhibition file is unlocked
	c.Check(hint, Equals, runinhibit.HintNotInhibited)
	// And snap lock is also unlocked
	testLock, err := snaplock.OpenLock("some-snap")
	c.Assert(err, IsNil)
	c.Check(testLock.TryLock(), IsNil)
	testLock.Close()
}

func (s *linkSnapSuite) TestDoKillSnapAppsTerminateBestEffort(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	si := &snap.SideInfo{
		RealName: "some-snap",
		Revision: snap.R(1),
	}
	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
		Current:  si.Revision,
		Active:   true,
	})

	s.fakeBackend.maybeInjectErr = func(op *fakeOp) error {
		if op.op == "kill-snap-apps:remove" {
			return fmt.Errorf("boom!")
		}
		return nil
	}

	task := s.state.NewTask("kill-snap-apps", "")
	task.Set("kill-reason", snap.KillReasonRemove)
	task.Set("snap-setup", &snapstate.SnapSetup{SideInfo: si})
	chg := s.state.NewChange("test", "")
	chg.AddTask(task)

	s.state.Unlock()

	s.se.Ensure()
	s.se.Wait()

	s.state.Lock()

	c.Assert(task.Status(), Equals, state.DoneStatus)

	hint, _, err := runinhibit.IsLocked("some-snap", nil)
	c.Assert(err, IsNil)
	// Error is ignored, inhibition lock is held
	c.Check(hint, Equals, runinhibit.HintInhibitedForRemove)
	// And snap lock is also unlocked
	testLock, err := snaplock.OpenLock("some-snap")
	c.Assert(err, IsNil)
	c.Check(testLock.TryLock(), IsNil)
	testLock.Close()
	// But a warning is emitted
	warnings := s.state.AllWarnings()
	c.Assert(warnings, HasLen, 1)
	c.Assert(warnings[0].String(), Equals, `cannot terminate running app processes for "some-snap": boom!`)
}

func (s *linkSnapSuite) testDoUndoKillSnapApps(c *C, svc bool) {
	s.state.Lock()
	defer s.state.Unlock()

	si := &snap.SideInfo{
		RealName: "some-snap",
		Revision: snap.R(1),
	}
	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
		Current:  si.Revision,
		Active:   true,
	})

	restore := snapstate.MockSnapReadInfo(func(name string, si *snap.SideInfo) (*snap.Info, error) {
		c.Assert(name, Equals, "some-snap")
		info := &snap.Info{SuggestedName: name, SideInfo: *si, SnapType: snap.TypeApp}
		if svc {
			info.Apps = map[string]*snap.AppInfo{
				"svc1": {Snap: info, Name: "svc1", Daemon: "simple"},
			}
		}
		return info, nil
	})
	defer restore()

	task := s.state.NewTask("kill-snap-apps", "")
	task.Set("kill-reason", snap.KillReasonRemove)
	task.Set("snap-setup", &snapstate.SnapSetup{SideInfo: si})
	chg := s.state.NewChange("test", "")
	chg.AddTask(task)

	terr := s.state.NewTask("error-trigger", "provoking total undo")
	terr.WaitFor(task)
	chg.AddTask(terr)

	s.state.Unlock()

	for i := 0; i < 6; i++ {
		s.se.Ensure()
		s.se.Wait()
	}

	s.state.Lock()

	c.Check(task.Status(), Equals, state.UndoneStatus)

	expected := fakeOps{
		{
			op:         "kill-snap-apps:remove",
			name:       "some-snap",
			snapLocked: true,
		},
	}
	if svc {
		expected = append(expected, fakeOp{
			op:   "stop-snap-services:remove",
			path: filepath.Join(dirs.SnapMountDir, "some-snap/1"),
		})
	}

	c.Assert(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Check(s.fakeBackend.ops, DeepEquals, expected)

	hint, _, err := runinhibit.IsLocked("some-snap", nil)
	c.Assert(err, IsNil)
	// On undo hint inhibition file is unlocked
	c.Check(hint, Equals, runinhibit.HintNotInhibited)
	// And snap lock is also unlocked
	testLock, err := snaplock.OpenLock("some-snap")
	c.Assert(err, IsNil)
	c.Check(testLock.TryLock(), IsNil)
	testLock.Close()
}

func (s *linkSnapSuite) TestDoUndoKillSnapApps(c *C) {
	const svc = false
	s.testDoUndoKillSnapApps(c, svc)
}

func (s *linkSnapSuite) TestDoUndoKillSnapAppsWithServices(c *C) {
	const svc = true
	s.testDoUndoKillSnapApps(c, svc)
}
