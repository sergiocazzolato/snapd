// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2017 Canonical Ltd
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
	"fmt"

	. "gopkg.in/check.v1"

	"github.com/snapcore/snapd/interfaces"
	"github.com/snapcore/snapd/interfaces/apparmor"
	"github.com/snapcore/snapd/interfaces/builtin"
	"github.com/snapcore/snapd/interfaces/seccomp"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/testutil"
)

type AccountControlSuite struct {
	iface    interfaces.Interface
	slotInfo *snap.SlotInfo
	slot     *interfaces.ConnectedSlot
	plugInfo *snap.PlugInfo
	plug     *interfaces.ConnectedPlug
}

var _ = Suite(&AccountControlSuite{
	iface: builtin.MustInterface("account-control"),
})

const accountCtlMockPlugSnapInfo = `name: other
version: 1.0
apps:
 app2:
  command: foo
  plugs: [account-control]
`

const accountCtlMockSlotSnapInfo = `name: core
version: 1.0
type: os
slots:
 account-control:
  interface: account-control
apps:
 app1:
`

func (s *AccountControlSuite) SetUpTest(c *C) {
	s.slot, s.slotInfo = MockConnectedSlot(c, accountCtlMockSlotSnapInfo, nil, "account-control")
	s.plug, s.plugInfo = MockConnectedPlug(c, accountCtlMockPlugSnapInfo, nil, "account-control")
}

func (s *AccountControlSuite) TestName(c *C) {
	c.Assert(s.iface.Name(), Equals, "account-control")
}

func (s *AccountControlSuite) TestSanitizeSlot(c *C) {
	c.Assert(interfaces.BeforePrepareSlot(s.iface, s.slotInfo), IsNil)
}

func (s *AccountControlSuite) TestSanitizePlug(c *C) {
	c.Assert(interfaces.BeforePreparePlug(s.iface, s.plugInfo), IsNil)
}

func (s *AccountControlSuite) TestUsedSecuritySystems(c *C) {
	// connected plugs have a non-nil security snippet for apparmor
	apparmorSpec := apparmor.NewSpecification(s.plug.AppSet())
	err := apparmorSpec.AddConnectedPlug(s.iface, s.plug, s.slot)
	c.Assert(err, IsNil)
	c.Assert(apparmorSpec.SecurityTags(), DeepEquals, []string{"snap.other.app2"})
	c.Assert(apparmorSpec.SnippetForTag("snap.other.app2"), testutil.Contains, "/{,usr/}sbin/chpasswd")

	seccompSpec := seccomp.NewSpecification(s.plug.AppSet())
	err = seccompSpec.AddConnectedPlug(s.iface, s.plug, s.slot)
	c.Assert(err, IsNil)
	c.Assert(seccompSpec.SecurityTags(), DeepEquals, []string{"snap.other.app2"})
	group, err := osutil.FindGidOwning("/etc/shadow")
	c.Assert(err, IsNil)
	c.Check(seccompSpec.SnippetForTag("snap.other.app2"), testutil.Contains,
		fmt.Sprintf("\nfchown - u:root %v\n", group))
}

func (s *AccountControlSuite) TestInterfaces(c *C) {
	c.Check(builtin.Interfaces(), testutil.DeepContains, s.iface)
}
