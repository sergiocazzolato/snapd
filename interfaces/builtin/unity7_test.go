// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2016 Canonical Ltd
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

package builtin_test

import (
	. "gopkg.in/check.v1"

	"github.com/snapcore/snapd/interfaces"
	"github.com/snapcore/snapd/interfaces/apparmor"
	"github.com/snapcore/snapd/interfaces/builtin"
	"github.com/snapcore/snapd/interfaces/seccomp"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/testutil"
)

type Unity7InterfaceSuite struct {
	iface        interfaces.Interface
	slotInfo     *snap.SlotInfo
	slot         *interfaces.ConnectedSlot
	plugInfo     *snap.PlugInfo
	plug         *interfaces.ConnectedPlug
	plugInstInfo *snap.PlugInfo
	plugInst     *interfaces.ConnectedPlug
}

var _ = Suite(&Unity7InterfaceSuite{
	iface: builtin.MustInterface("unity7"),
})

const unity7mockPlugSnapInfoYaml = `name: other-snap
version: 1.0
apps:
 app2:
  command: foo
  plugs: [unity7]
`

const unity7mockSlotSnapInfoYaml = `name: core
version: 1.0
type: os
slots:
 unity7:
  interface: unity7
`

func (s *Unity7InterfaceSuite) SetUpTest(c *C) {
	s.slot, s.slotInfo = MockConnectedSlot(c, unity7mockSlotSnapInfoYaml, nil, "unity7")
	s.plug, s.plugInfo = MockConnectedPlug(c, unity7mockPlugSnapInfoYaml, nil, "unity7")
	s.plugInst, s.plugInstInfo = MockConnectedPlug(c, unity7mockPlugSnapInfoYaml, nil, "unity7")
	s.plugInst.AppSet().Info().InstanceKey = "instance"
}

func (s *Unity7InterfaceSuite) TestName(c *C) {
	c.Assert(s.iface.Name(), Equals, "unity7")
}

func (s *Unity7InterfaceSuite) TestSanitizeSlot(c *C) {
	c.Assert(interfaces.BeforePrepareSlot(s.iface, s.slotInfo), IsNil)
}

func (s *Unity7InterfaceSuite) TestSanitizePlug(c *C) {
	c.Assert(interfaces.BeforePreparePlug(s.iface, s.plugInfo), IsNil)
}

func (s *Unity7InterfaceSuite) TestUsedSecuritySystems(c *C) {
	// connected plugs have a non-nil security snippet for apparmor
	apparmorSpec := apparmor.NewSpecification(s.plug.AppSet())
	err := apparmorSpec.AddConnectedPlug(s.iface, s.plug, s.slot)
	c.Assert(err, IsNil)
	c.Assert(apparmorSpec.SecurityTags(), DeepEquals, []string{"snap.other-snap.app2"})
	c.Assert(apparmorSpec.SnippetForTag("snap.other-snap.app2"), testutil.Contains, `/usr/share/pixmaps`)
	c.Assert(apparmorSpec.SnippetForTag("snap.other-snap.app2"), testutil.Contains, `path=/com/canonical/indicator/messages/other_snap_*_desktop`)
	c.Assert(apparmorSpec.SnippetForTag("snap.other-snap.app2"), testutil.Contains, `deny /var/lib/snapd/desktop/applications/mimeinfo.cache r,`)

	// getDesktopFileRules() rules
	c.Assert(apparmorSpec.SnippetForTag("snap.other-snap.app2"), testutil.Contains, `# This leaks the names of snaps with desktop files`)
	c.Assert(apparmorSpec.SnippetForTag("snap.other-snap.app2"), testutil.Contains, `/var/lib/snapd/desktop/applications/ r,`)

	// connected plugs for instance name have a non-nil security snippet for apparmor
	apparmorSpec = apparmor.NewSpecification(s.plugInst.AppSet())
	err = apparmorSpec.AddConnectedPlug(s.iface, s.plugInst, s.slot)
	c.Assert(err, IsNil)
	c.Assert(apparmorSpec.SecurityTags(), DeepEquals, []string{"snap.other-snap_instance.app2"})
	c.Assert(apparmorSpec.SnippetForTag("snap.other-snap_instance.app2"), testutil.Contains, `/usr/share/pixmaps`)
	c.Assert(apparmorSpec.SnippetForTag("snap.other-snap_instance.app2"), testutil.Contains, `path=/com/canonical/indicator/messages/other_snap_instance_*_desktop`)

	// connected plugs for instance name have a non-nil security snippet for apparmor
	apparmorSpec = apparmor.NewSpecification(s.plugInst.AppSet())
	err = apparmorSpec.AddConnectedPlug(s.iface, s.plugInst, s.slot)
	c.Assert(err, IsNil)
	c.Assert(apparmorSpec.SecurityTags(), DeepEquals, []string{"snap.other-snap_instance.app2"})
	c.Assert(apparmorSpec.SnippetForTag("snap.other-snap_instance.app2"), testutil.Contains, `/usr/share/pixmaps`)
	c.Assert(apparmorSpec.SnippetForTag("snap.other-snap_instance.app2"), testutil.Contains, `path=/com/canonical/indicator/messages/other_snap_instance_*_desktop`)

	// connected plugs have a non-nil security snippet for seccomp
	seccompSpec := seccomp.NewSpecification(s.plug.AppSet())
	err = seccompSpec.AddConnectedPlug(s.iface, s.plug, s.slot)
	c.Assert(err, IsNil)
	c.Assert(seccompSpec.SecurityTags(), DeepEquals, []string{"snap.other-snap.app2"})
	c.Check(seccompSpec.SnippetForTag("snap.other-snap.app2"), testutil.Contains, "bind\n")
}

func (s *Unity7InterfaceSuite) TestInterfaces(c *C) {
	c.Check(builtin.Interfaces(), testutil.DeepContains, s.iface)
}

// Test how unity7 interface interacts desktop-file-ids attribute in desktop interface.
var _ = Suite(&desktopFileRulesBaseSuite{
	iface:    "unity7",
	slotYaml: unity7mockSlotSnapInfoYaml,
})
