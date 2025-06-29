// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2015-2022 Canonical Ltd
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

package asserts

import (
	"bytes"
	"crypto"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	// expected for digests
	_ "golang.org/x/crypto/sha3"

	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/release"
	"github.com/snapcore/snapd/snap/naming"
	"github.com/snapcore/snapd/strutil"
)

// SnapDeclaration holds a snap-declaration assertion, declaring a
// snap binding its identifying snap-id to a name, asserting its
// publisher and its other properties.
type SnapDeclaration struct {
	assertionBase
	refreshControl      []string
	plugRules           map[string]*PlugRule
	slotRules           map[string]*SlotRule
	autoAliases         []string
	aliases             map[string]string
	revisionAuthorities []*RevisionAuthority
	timestamp           time.Time
}

// Series returns the series for which the snap is being declared.
func (snapdcl *SnapDeclaration) Series() string {
	return snapdcl.HeaderString("series")
}

// SnapID returns the snap id of the declared snap.
func (snapdcl *SnapDeclaration) SnapID() string {
	return snapdcl.HeaderString("snap-id")
}

// SnapName returns the declared snap name.
func (snapdcl *SnapDeclaration) SnapName() string {
	return snapdcl.HeaderString("snap-name")
}

// PublisherID returns the identifier of the publisher of the declared snap.
func (snapdcl *SnapDeclaration) PublisherID() string {
	return snapdcl.HeaderString("publisher-id")
}

// Timestamp returns the time when the snap-declaration was issued.
func (snapdcl *SnapDeclaration) Timestamp() time.Time {
	return snapdcl.timestamp
}

// RefreshControl returns the ids of snaps whose updates are controlled by this declaration.
func (snapdcl *SnapDeclaration) RefreshControl() []string {
	return snapdcl.refreshControl
}

// PlugRule returns the plug-side rule about the given interface if one was included in the plugs stanza of the declaration, otherwise it returns nil.
func (snapdcl *SnapDeclaration) PlugRule(interfaceName string) *PlugRule {
	return snapdcl.plugRules[interfaceName]
}

// SlotRule returns the slot-side rule about the given interface if one was included in the slots stanza of the declaration, otherwise it returns nil.
func (snapdcl *SnapDeclaration) SlotRule(interfaceName string) *SlotRule {
	return snapdcl.slotRules[interfaceName]
}

// AutoAliases returns the optional auto-aliases granted to this snap.
// XXX: deprecated, will go away
func (snapdcl *SnapDeclaration) AutoAliases() []string {
	return snapdcl.autoAliases
}

// Aliases returns the optional explicit aliases granted to this snap.
func (snapdcl *SnapDeclaration) Aliases() map[string]string {
	return snapdcl.aliases
}

// RevisionAuthority return any revision authority entries matching the given
// provenance.
func (snapdcl *SnapDeclaration) RevisionAuthority(provenance string) []*RevisionAuthority {
	res := make([]*RevisionAuthority, 0, 1)
	for _, ra := range snapdcl.revisionAuthorities {
		if strutil.ListContains(ra.Provenance, provenance) {
			res = append(res, ra)
		}
	}
	if len(res) == 0 {
		return nil
	}
	return res
}

// Implement further consistency checks.
func (snapdcl *SnapDeclaration) checkConsistency(db RODatabase, acck *AccountKey) error {
	if !db.IsTrustedAccount(snapdcl.AuthorityID()) {
		return fmt.Errorf("snap-declaration assertion for %q (id %q) is not signed by a directly trusted authority: %s", snapdcl.SnapName(), snapdcl.SnapID(), snapdcl.AuthorityID())
	}
	_, err := db.Find(AccountType, map[string]string{
		"account-id": snapdcl.PublisherID(),
	})
	if errors.Is(err, &NotFoundError{}) {
		return fmt.Errorf("snap-declaration assertion for %q (id %q) does not have a matching account assertion for the publisher %q", snapdcl.SnapName(), snapdcl.SnapID(), snapdcl.PublisherID())
	}
	if err != nil {
		return err
	}

	return nil
}

// expected interface is implemented
var _ consistencyChecker = (*SnapDeclaration)(nil)

// Prerequisites returns references to this snap-declaration's prerequisite assertions.
func (snapdcl *SnapDeclaration) Prerequisites() []*Ref {
	return []*Ref{
		{Type: AccountType, PrimaryKey: []string{snapdcl.PublisherID()}},
	}
}

func compilePlugRules(plugs map[string]any, compiled func(iface string, plugRule *PlugRule)) error {
	for iface, rule := range plugs {
		plugRule, err := compilePlugRule(iface, rule)
		if err != nil {
			return err
		}
		compiled(iface, plugRule)
	}
	return nil
}

func compileSlotRules(slots map[string]any, compiled func(iface string, slotRule *SlotRule)) error {
	for iface, rule := range slots {
		slotRule, err := compileSlotRule(iface, rule)
		if err != nil {
			return err
		}
		compiled(iface, slotRule)
	}
	return nil
}

func snapDeclarationFormatAnalyze(headers map[string]any, body []byte) (formatnum int, err error) {
	_, plugsOk := headers["plugs"]
	_, slotsOk := headers["slots"]
	if !(plugsOk || slotsOk) {
		return 0, nil
	}

	formatnum = 1
	setFormatNum := func(num int) {
		if num > formatnum {
			formatnum = num
		}
	}

	plugs, err := checkMap(headers, "plugs")
	if err != nil {
		return 0, err
	}
	err = compilePlugRules(plugs, func(_ string, rule *PlugRule) {
		if rule.feature(dollarAttrConstraintsFeature) {
			setFormatNum(2)
		}
		if rule.feature(deviceScopeConstraintsFeature) {
			setFormatNum(3)
		}
		if rule.feature(nameConstraintsFeature) {
			setFormatNum(4)
		}
		if rule.feature(altAttrMatcherFeature) {
			setFormatNum(5)
		}
		if rule.feature(publisherIDConstraintsFeature) {
			setFormatNum(6)
		}
	})
	if err != nil {
		return 0, err
	}

	slots, err := checkMap(headers, "slots")
	if err != nil {
		return 0, err
	}
	err = compileSlotRules(slots, func(_ string, rule *SlotRule) {
		if rule.feature(dollarAttrConstraintsFeature) {
			setFormatNum(2)
		}
		if rule.feature(deviceScopeConstraintsFeature) {
			setFormatNum(3)
		}
		if rule.feature(nameConstraintsFeature) {
			setFormatNum(4)
		}
		if rule.feature(altAttrMatcherFeature) {
			setFormatNum(5)
		}
		if rule.feature(publisherIDConstraintsFeature) {
			setFormatNum(6)
		}
	})
	if err != nil {
		return 0, err
	}

	return formatnum, nil
}

func checkAliases(headers map[string]any) (map[string]string, error) {
	value, ok := headers["aliases"]
	if !ok {
		return nil, nil
	}
	aliasList, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf(`"aliases" header must be a list of alias maps`)
	}
	if len(aliasList) == 0 {
		return nil, nil
	}

	aliasMap := make(map[string]string, len(aliasList))
	for i, item := range aliasList {
		aliasItem, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf(`"aliases" header must be a list of alias maps`)
		}

		what := fmt.Sprintf(`in "aliases" item %d`, i+1)
		name, err := checkStringMatchesWhat(aliasItem, "name", what, naming.ValidAlias)
		if err != nil {
			return nil, err
		}

		what = fmt.Sprintf(`for alias %q`, name)
		target, err := checkStringMatchesWhat(aliasItem, "target", what, naming.ValidApp)
		if err != nil {
			return nil, err
		}

		if _, ok := aliasMap[name]; ok {
			return nil, fmt.Errorf(`duplicated definition in "aliases" for alias %q`, name)
		}

		aliasMap[name] = target
	}

	return aliasMap, nil
}

func assembleSnapDeclaration(assert assertionBase) (Assertion, error) {
	_, err := checkExistsString(assert.headers, "snap-name")
	if err != nil {
		return nil, err
	}

	_, err = checkNotEmptyString(assert.headers, "publisher-id")
	if err != nil {
		return nil, err
	}

	timestamp, err := checkRFC3339Date(assert.headers, "timestamp")
	if err != nil {
		return nil, err
	}

	var refControl []string
	var plugRules map[string]*PlugRule
	var slotRules map[string]*SlotRule

	refControl, err = checkStringList(assert.headers, "refresh-control")
	if err != nil {
		return nil, err
	}

	plugs, err := checkMap(assert.headers, "plugs")
	if err != nil {
		return nil, err
	}
	if plugs != nil {
		plugRules = make(map[string]*PlugRule, len(plugs))
		err := compilePlugRules(plugs, func(iface string, rule *PlugRule) {
			plugRules[iface] = rule
		})
		if err != nil {
			return nil, err
		}
	}

	slots, err := checkMap(assert.headers, "slots")
	if err != nil {
		return nil, err
	}
	if slots != nil {
		slotRules = make(map[string]*SlotRule, len(slots))
		err := compileSlotRules(slots, func(iface string, rule *SlotRule) {
			slotRules[iface] = rule
		})
		if err != nil {
			return nil, err
		}
	}

	// XXX: depracated, will go away later
	autoAliases, err := checkStringListMatches(assert.headers, "auto-aliases", naming.ValidAlias)
	if err != nil {
		return nil, err
	}

	aliases, err := checkAliases(assert.headers)
	if err != nil {
		return nil, err
	}

	var ras []*RevisionAuthority

	ra, ok := assert.headers["revision-authority"]
	if ok {
		ramaps, ok := ra.([]any)
		if !ok {
			return nil, fmt.Errorf("revision-authority stanza must be a list of maps")
		}
		if len(ramaps) == 0 {
			// there is no syntax producing this scenario but be robust
			return nil, fmt.Errorf("revision-authority stanza cannot be empty")
		}
		ras = make([]*RevisionAuthority, 0, len(ramaps))
		for _, ramap := range ramaps {
			m, ok := ramap.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("revision-authority stanza must be a list of maps")
			}
			accountID, err := checkStringMatchesWhat(m, "account-id", "in revision authority", validAccountID)
			if err != nil {
				return nil, err
			}
			prov, err := checkStringListInMap(m, "provenance", "provenance in revision authority", naming.ValidProvenance)
			if err != nil {
				return nil, err
			}
			if len(prov) == 0 {
				return nil, fmt.Errorf("provenance in revision authority cannot be empty")
			}
			minRevision := 1
			maxRevision := 0
			if _, ok := m["min-revision"]; ok {
				var err error
				minRevision, err = checkSnapRevisionWhat(m, "min-revision", "in revision authority")
				if err != nil {
					return nil, err
				}
			}
			if _, ok := m["max-revision"]; ok {
				var err error
				maxRevision, err = checkSnapRevisionWhat(m, "max-revision", "in revision authority")
				if err != nil {
					return nil, err
				}
			}
			if maxRevision != 0 && maxRevision < minRevision {
				return nil, fmt.Errorf("optional max-revision cannot be less than min-revision in revision-authority")
			}
			devscope, err := compileDeviceScopeConstraint(m, "revision-authority")
			if err != nil {
				return nil, err
			}
			ras = append(ras, &RevisionAuthority{
				AccountID:   accountID,
				Provenance:  prov,
				MinRevision: minRevision,
				MaxRevision: maxRevision,
				DeviceScope: devscope,
			})
		}

	}

	return &SnapDeclaration{
		assertionBase:       assert,
		refreshControl:      refControl,
		plugRules:           plugRules,
		slotRules:           slotRules,
		autoAliases:         autoAliases,
		aliases:             aliases,
		revisionAuthorities: ras,
		timestamp:           timestamp,
	}, nil
}

// RevisionAuthority holds information about an account that can sign revisions
// for a given snap.
type RevisionAuthority struct {
	AccountID  string
	Provenance []string

	MinRevision int
	MaxRevision int

	DeviceScope *DeviceScopeConstraint
}

func (ra *RevisionAuthority) checkProvenanceAndRevision(a interface {
	Assertion
	Provenance() string
}, what string, revno int, model *Model, store *Store) error {
	if !strutil.ListContains(ra.Provenance, a.Provenance()) {
		return fmt.Errorf("provenance mismatch")
	}
	if a.AuthorityID() != ra.AccountID {
		return fmt.Errorf("authority-id mismatch")
	}
	if revno < ra.MinRevision {
		return fmt.Errorf("%s revision %d is less than min-revision %d", what, revno, ra.MinRevision)
	}
	if ra.MaxRevision != 0 && revno > ra.MaxRevision {
		return fmt.Errorf("%s revision %d is greater than max-revision %d", what, revno, ra.MaxRevision)
	}
	if ra.DeviceScope != nil && model != nil {
		opts := DeviceScopeConstraintCheckOptions{UseFriendlyStores: true}
		if err := ra.DeviceScope.Check(model, store, &opts); err != nil {
			return err
		}
	}
	return nil
}

// Check tests whether rev matches the revision authority constraints.
// Optional model and store must be provided to cross-check device-specific
// constraints.
func (ra *RevisionAuthority) Check(rev *SnapRevision, model *Model, store *Store) error {
	return ra.checkProvenanceAndRevision(rev, "snap", rev.SnapRevision(), model, store)
}

// CheckResourceRevision tests whether resrev matches the revision authority
// constraints. Optional model and store must be provided to cross-check
// device-specific constraints.
func (ra *RevisionAuthority) CheckResourceRevision(resrev *SnapResourceRevision, model *Model, store *Store) error {
	return ra.checkProvenanceAndRevision(resrev, "resource", resrev.ResourceRevision(), model, store)
}

var validSnapIntegrityTypes = []string{"dm-verity"}

var validVersionsForIntegrityType = map[string][]int{
	// version 1 corresponds to dm-verity format 1
	"dm-verity": {1},
}

var validHashAlgorithmsForIntegrityType = map[string][]string{
	// kernel supported algorithms:
	// https://gitlab.com/cryptsetup/cryptsetup/-/blob/main/lib/crypto_backend/crypto_kernel.c?ref_type=heads#L35
	// Go crypto's supported algorithms:
	// https://cs.opensource.google/go/go/+/refs/tags/go1.23.4:src/crypto/crypto.go;l=68
	"dm-verity": {
		"sha256",
	},
}

func contains[V int | string](l []V, i V) bool {
	for _, v := range l {
		if v == i {
			return true
		}
	}

	return false
}

func toHash(s string) crypto.Hash {
	switch s {
	case "sha256":
		return crypto.SHA256
	default:
		return 0
	}
}

// SnapIntegrityData holds information about integrity data of a specific type included in a snap's revision.
//
// A single snap revision can have multiple variants of integrity data which are represented as an array in the
// snap revision assertion.
type SnapIntegrityData struct {
	Type          string
	Version       uint
	HashAlg       string
	DataBlockSize uint
	HashBlockSize uint
	Digest        string
	Salt          string
}

// SnapFileSHA3_384 computes the SHA3-384 digest of the given snap file.
// It also returns its size.
func SnapFileSHA3_384(snapPath string) (digest string, size uint64, err error) {
	sha3_384Dgst, size, err := osutil.FileDigest(snapPath, crypto.SHA3_384)
	if err != nil {
		return "", 0, fmt.Errorf("cannot compute snap %q digest: %v", snapPath, err)
	}

	sha3_384, err := EncodeDigest(crypto.SHA3_384, sha3_384Dgst)
	if err != nil {
		return "", 0, fmt.Errorf("cannot encode snap %q digest: %v", snapPath, err)
	}
	return sha3_384, size, nil
}

// SnapBuild holds a snap-build assertion, asserting the properties of a snap
// at the time it was built by the developer.
type SnapBuild struct {
	assertionBase
	size      uint64
	timestamp time.Time
}

// SnapSHA3_384 returns the SHA3-384 digest of the snap.
func (snapbld *SnapBuild) SnapSHA3_384() string {
	return snapbld.HeaderString("snap-sha3-384")
}

// SnapID returns the snap id of the snap.
func (snapbld *SnapBuild) SnapID() string {
	return snapbld.HeaderString("snap-id")
}

// SnapSize returns the size of the snap.
func (snapbld *SnapBuild) SnapSize() uint64 {
	return snapbld.size
}

// Grade returns the grade of the snap: devel|stable
func (snapbld *SnapBuild) Grade() string {
	return snapbld.HeaderString("grade")
}

// Timestamp returns the time when the snap-build assertion was created.
func (snapbld *SnapBuild) Timestamp() time.Time {
	return snapbld.timestamp
}

func assembleSnapBuild(assert assertionBase) (Assertion, error) {
	_, err := checkDigest(assert.headers, "snap-sha3-384", crypto.SHA3_384)
	if err != nil {
		return nil, err
	}

	_, err = checkNotEmptyString(assert.headers, "snap-id")
	if err != nil {
		return nil, err
	}

	_, err = checkNotEmptyString(assert.headers, "grade")
	if err != nil {
		return nil, err
	}

	size, err := checkUint(assert.headers, "snap-size", 64)
	if err != nil {
		return nil, err
	}

	timestamp, err := checkRFC3339Date(assert.headers, "timestamp")
	if err != nil {
		return nil, err
	}
	// ignore extra headers and non-empty body for future compatibility
	return &SnapBuild{
		assertionBase: assert,
		size:          size,
		timestamp:     timestamp,
	}, nil
}

// SnapRevision holds a snap-revision assertion, which is a statement by the
// store acknowledging the receipt of a build of a snap and labeling it with a
// snap revision.
type SnapRevision struct {
	assertionBase
	snapSize     uint64
	snapRevision int
	timestamp    time.Time

	snapIntegrityData []SnapIntegrityData
}

// SnapSHA3_384 returns the SHA3-384 digest of the snap.
func (snaprev *SnapRevision) SnapSHA3_384() string {
	return snaprev.HeaderString("snap-sha3-384")
}

// Provenance returns the optional provenance of the snap (defaults to
// global-upload (naming.DefaultProvenance)).
func (snaprev *SnapRevision) Provenance() string {
	return snaprev.HeaderString("provenance")
}

// SnapID returns the snap id of the snap.
func (snaprev *SnapRevision) SnapID() string {
	return snaprev.HeaderString("snap-id")
}

// SnapSize returns the size in bytes of the snap submitted to the store.
func (snaprev *SnapRevision) SnapSize() uint64 {
	return snaprev.snapSize
}

// SnapRevision returns the revision assigned to this build of the snap.
func (snaprev *SnapRevision) SnapRevision() int {
	return snaprev.snapRevision
}

// DeveloperID returns the id of the developer that submitted this build of the
// snap.
func (snaprev *SnapRevision) DeveloperID() string {
	return snaprev.HeaderString("developer-id")
}

// Timestamp returns the time when the snap-revision was issued.
func (snaprev *SnapRevision) Timestamp() time.Time {
	return snaprev.timestamp
}

// SnapIntegrityData returns the snap integrity data associated with the snap revision if any.
func (snaprev *SnapRevision) SnapIntegrityData() []SnapIntegrityData {
	return snaprev.snapIntegrityData
}

// Implement further consistency checks.
func (snaprev *SnapRevision) checkConsistency(db RODatabase, acck *AccountKey) error {
	otherProvenance := snaprev.Provenance() != naming.DefaultProvenance
	if !otherProvenance && !db.IsTrustedAccount(snaprev.AuthorityID()) {
		// delegating global-upload revisions is not allowed
		return fmt.Errorf("snap-revision assertion for snap id %q is not signed by a store: %s", snaprev.SnapID(), snaprev.AuthorityID())
	}
	_, err := db.Find(AccountType, map[string]string{
		"account-id": snaprev.DeveloperID(),
	})
	if errors.Is(err, &NotFoundError{}) {
		return fmt.Errorf("snap-revision assertion for snap id %q does not have a matching account assertion for the developer %q", snaprev.SnapID(), snaprev.DeveloperID())
	}
	if err != nil {
		return err
	}
	a, err := db.Find(SnapDeclarationType, map[string]string{
		// XXX: mediate getting current series through some context object? this gets the job done for now
		"series":  release.Series,
		"snap-id": snaprev.SnapID(),
	})
	if errors.Is(err, &NotFoundError{}) {
		return fmt.Errorf("snap-revision assertion for snap id %q does not have a matching snap-declaration assertion", snaprev.SnapID())
	}
	if err != nil {
		return err
	}
	if otherProvenance {
		decl := a.(*SnapDeclaration)
		ras := decl.RevisionAuthority(snaprev.Provenance())
		matchingRevAuthority := false
		for _, ra := range ras {
			// model==store==nil, we do not perform device-specific
			// checks at this level, those are performed at
			// higher-level guarding installing actual snaps
			if err := ra.Check(snaprev, nil, nil); err == nil {
				matchingRevAuthority = true
				break
			}
		}
		if !matchingRevAuthority {
			return fmt.Errorf("snap-revision assertion with provenance %q for snap id %q is not signed by an authorized authority: %s", snaprev.Provenance(), snaprev.SnapID(), snaprev.AuthorityID())
		}
	}
	return nil
}

// expected interface is implemented
var _ consistencyChecker = (*SnapRevision)(nil)

// Prerequisites returns references to this snap-revision's prerequisite assertions.
func (snaprev *SnapRevision) Prerequisites() []*Ref {
	return []*Ref{
		// XXX: mediate getting current series through some context object? this gets the job done for now
		{Type: SnapDeclarationType, PrimaryKey: []string{release.Series, snaprev.SnapID()}},
		{Type: AccountType, PrimaryKey: []string{snaprev.DeveloperID()}},
	}
}

func checkSnapRevisionWhat(headers map[string]any, name, what string) (snapRevision int, err error) {
	snapRevision, err = checkIntWhat(headers, name, what)
	if err != nil {
		return 0, err
	}
	if snapRevision < 1 {
		return 0, fmt.Errorf(`%q %s must be >=1: %d`, name, what, snapRevision)
	}
	return snapRevision, nil
}

func checkOptionalSnapRevisionWhat(headers map[string]any, name, what string) (snapRevision int, err error) {
	if _, ok := headers[name]; !ok {
		return 0, nil
	}
	return checkSnapRevisionWhat(headers, name, what)
}

func checkSnapIntegrity(headers map[string]any) ([]SnapIntegrityData, error) {
	value, ok := headers["integrity"]
	if !ok {
		// integrity stanzas are optional
		return nil, nil
	}

	integrityList, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf(`"integrity" header must contain a list of integrity data`)
	}
	if len(integrityList) == 0 {
		return nil, nil
	}

	var snapIntegrityDataList []SnapIntegrityData

	for i, il := range integrityList {
		id, ok := il.(map[string]any)
		if !ok {
			return nil, fmt.Errorf(`"integrity" header must contain a list of integrity data`)
		}

		what := fmt.Sprintf("of integrity data [%d]", i)
		typ, err := checkExistsStringWhat(id, "type", what)
		if err != nil {
			return nil, err
		}

		if !contains(validSnapIntegrityTypes, typ) {
			return nil, fmt.Errorf("\"type\" of integrity data [%d] must be one of (%s)", i, strings.Join(validSnapIntegrityTypes, "|"))
		}

		what = fmt.Sprintf("of integrity data [%d] of type %q", i, typ)
		version, err := checkUintWhat(id, "version", 64, what)
		if err != nil {
			return nil, err
		}

		if !contains(validVersionsForIntegrityType[typ], int(version)) {
			return nil, fmt.Errorf(`version of integrity data [%d] of type %q must be one of %v`, i, typ, validVersionsForIntegrityType[typ])
		}

		alg, err := checkExistsStringWhat(id, "hash-algorithm", what)
		if err != nil {
			return nil, err
		}

		if !contains(validHashAlgorithmsForIntegrityType[typ], alg) {
			return nil, fmt.Errorf(`hash algorithm of integrity data [%d] of type %q must be one of %v`, i, typ, validHashAlgorithmsForIntegrityType[typ])
		}

		what = fmt.Sprintf("of integrity data [%d] of type %q (%s)", i, typ, alg)
		dataBlockSize, err := checkUintWhat(id, "data-block-size", 64, what)
		if err != nil {
			return nil, err
		}

		hashBlockSize, err := checkUintWhat(id, "hash-block-size", 64, what)
		if err != nil {
			return nil, err
		}

		h := toHash(alg)
		encDigest, err := checkDigestDecWhat(id, "digest", h, hex.DecodeString, what)
		if err != nil {
			return nil, err
		}

		encSalt, err := checkDigestDecWhat(id, "salt", h, hex.DecodeString, what)
		if err != nil {
			return nil, err
		}

		snapIntegrityData := SnapIntegrityData{
			Type:          typ,
			Version:       uint(version),
			HashAlg:       alg,
			DataBlockSize: uint(dataBlockSize),
			HashBlockSize: uint(hashBlockSize),
			Digest:        encDigest,
			Salt:          encSalt,
		}

		snapIntegrityDataList = append(snapIntegrityDataList, snapIntegrityData)
	}

	return snapIntegrityDataList, nil
}

func assembleSnapRevision(assert assertionBase) (Assertion, error) {
	_, err := checkDigest(assert.headers, "snap-sha3-384", crypto.SHA3_384)
	if err != nil {
		return nil, err
	}

	_, err = checkStringMatches(assert.headers, "provenance", naming.ValidProvenance)
	if err != nil {
		return nil, err
	}

	_, err = checkNotEmptyString(assert.headers, "snap-id")
	if err != nil {
		return nil, err
	}

	snapSize, err := checkUint(assert.headers, "snap-size", 64)
	if err != nil {
		return nil, err
	}

	snapRevision, err := checkSnapRevisionWhat(assert.headers, "snap-revision", "header")
	if err != nil {
		return nil, err
	}

	_, err = checkNotEmptyString(assert.headers, "developer-id")
	if err != nil {
		return nil, err
	}

	timestamp, err := checkRFC3339Date(assert.headers, "timestamp")
	if err != nil {
		return nil, err
	}

	snapIntegrityData, err := checkSnapIntegrity(assert.headers)
	if err != nil {
		return nil, err
	}

	return &SnapRevision{
		assertionBase:     assert,
		snapSize:          snapSize,
		snapRevision:      snapRevision,
		timestamp:         timestamp,
		snapIntegrityData: snapIntegrityData,
	}, nil
}

// Validation holds a validation assertion, describing that a combination of
// (snap-id, approved-snap-id, approved-revision) has been validated for
// the series, meaning updating to that revision of approved-snap-id
// has been approved by the owner of the gating snap with snap-id.
type Validation struct {
	assertionBase
	revoked              bool
	timestamp            time.Time
	approvedSnapRevision int
}

// Series returns the series for which the validation holds.
func (validation *Validation) Series() string {
	return validation.HeaderString("series")
}

// SnapID returns the ID of the gating snap.
func (validation *Validation) SnapID() string {
	return validation.HeaderString("snap-id")
}

// ApprovedSnapID returns the ID of the gated snap.
func (validation *Validation) ApprovedSnapID() string {
	return validation.HeaderString("approved-snap-id")
}

// ApprovedSnapRevision returns the approved revision of the gated snap.
func (validation *Validation) ApprovedSnapRevision() int {
	return validation.approvedSnapRevision
}

// Revoked returns true if the validation has been revoked.
func (validation *Validation) Revoked() bool {
	return validation.revoked
}

// Timestamp returns the time when the validation was issued.
func (validation *Validation) Timestamp() time.Time {
	return validation.timestamp
}

// Implement further consistency checks.
func (validation *Validation) checkConsistency(db RODatabase, acck *AccountKey) error {
	_, err := db.Find(SnapDeclarationType, map[string]string{
		"series":  validation.Series(),
		"snap-id": validation.ApprovedSnapID(),
	})
	if errors.Is(err, &NotFoundError{}) {
		return fmt.Errorf("validation assertion by snap-id %q does not have a matching snap-declaration assertion for approved-snap-id %q", validation.SnapID(), validation.ApprovedSnapID())
	}
	if err != nil {
		return err
	}
	a, err := db.Find(SnapDeclarationType, map[string]string{
		"series":  validation.Series(),
		"snap-id": validation.SnapID(),
	})
	if errors.Is(err, &NotFoundError{}) {
		return fmt.Errorf("validation assertion by snap-id %q does not have a matching snap-declaration assertion", validation.SnapID())
	}
	if err != nil {
		return err
	}

	gatingDecl := a.(*SnapDeclaration)
	if gatingDecl.PublisherID() != validation.AuthorityID() {
		return fmt.Errorf("validation assertion by snap %q (id %q) not signed by its publisher", gatingDecl.SnapName(), validation.SnapID())
	}

	return nil
}

// expected interface is implemented
var _ consistencyChecker = (*Validation)(nil)

// Prerequisites returns references to this validation's prerequisite assertions.
func (validation *Validation) Prerequisites() []*Ref {
	return []*Ref{
		{Type: SnapDeclarationType, PrimaryKey: []string{validation.Series(), validation.SnapID()}},
		{Type: SnapDeclarationType, PrimaryKey: []string{validation.Series(), validation.ApprovedSnapID()}},
	}
}

func assembleValidation(assert assertionBase) (Assertion, error) {
	approvedSnapRevision, err := checkSnapRevisionWhat(assert.headers, "approved-snap-revision", "header")
	if err != nil {
		return nil, err
	}

	revoked, err := checkOptionalBool(assert.headers, "revoked")
	if err != nil {
		return nil, err
	}

	timestamp, err := checkRFC3339Date(assert.headers, "timestamp")
	if err != nil {
		return nil, err
	}

	return &Validation{
		assertionBase:        assert,
		revoked:              revoked,
		timestamp:            timestamp,
		approvedSnapRevision: approvedSnapRevision,
	}, nil
}

// BaseDeclaration holds a base-declaration assertion, declaring the
// policies (to start with interface ones) applying to all snaps of
// a series.
type BaseDeclaration struct {
	assertionBase
	plugRules map[string]*PlugRule
	slotRules map[string]*SlotRule
	timestamp time.Time
}

// Series returns the series whose snaps are governed by the declaration.
func (basedcl *BaseDeclaration) Series() string {
	return basedcl.HeaderString("series")
}

// Timestamp returns the time when the base-declaration was issued.
func (basedcl *BaseDeclaration) Timestamp() time.Time {
	return basedcl.timestamp
}

// PlugRule returns the plug-side rule about the given interface if one was included in the plugs stanza of the declaration, otherwise it returns nil.
func (basedcl *BaseDeclaration) PlugRule(interfaceName string) *PlugRule {
	return basedcl.plugRules[interfaceName]
}

// SlotRule returns the slot-side rule about the given interface if one was included in the slots stanza of the declaration, otherwise it returns nil.
func (basedcl *BaseDeclaration) SlotRule(interfaceName string) *SlotRule {
	return basedcl.slotRules[interfaceName]
}

// Implement further consistency checks.
func (basedcl *BaseDeclaration) checkConsistency(db RODatabase, acck *AccountKey) error {
	// XXX: not signed or stored yet in a db, but being ready for that
	if !db.IsTrustedAccount(basedcl.AuthorityID()) {
		return fmt.Errorf("base-declaration assertion for series %s is not signed by a directly trusted authority: %s", basedcl.Series(), basedcl.AuthorityID())
	}
	return nil
}

// expected interface is implemented
var _ consistencyChecker = (*BaseDeclaration)(nil)

func assembleBaseDeclaration(assert assertionBase) (Assertion, error) {
	var plugRules map[string]*PlugRule
	plugs, err := checkMap(assert.headers, "plugs")
	if err != nil {
		return nil, err
	}
	if plugs != nil {
		plugRules = make(map[string]*PlugRule, len(plugs))
		err := compilePlugRules(plugs, func(iface string, rule *PlugRule) {
			plugRules[iface] = rule
		})
		if err != nil {
			return nil, err
		}
	}

	var slotRules map[string]*SlotRule
	slots, err := checkMap(assert.headers, "slots")
	if err != nil {
		return nil, err
	}
	if slots != nil {
		slotRules = make(map[string]*SlotRule, len(slots))
		err := compileSlotRules(slots, func(iface string, rule *SlotRule) {
			slotRules[iface] = rule
		})
		if err != nil {
			return nil, err
		}
	}

	timestamp, err := checkRFC3339Date(assert.headers, "timestamp")
	if err != nil {
		return nil, err
	}

	return &BaseDeclaration{
		assertionBase: assert,
		plugRules:     plugRules,
		slotRules:     slotRules,
		timestamp:     timestamp,
	}, nil
}

var builtinBaseDeclaration *BaseDeclaration

// BuiltinBaseDeclaration exposes the initialized builtin base-declaration assertion. This is used by overlord/assertstate, other code should use assertstate.BaseDeclaration.
func BuiltinBaseDeclaration() *BaseDeclaration {
	return builtinBaseDeclaration
}

var (
	builtinBaseDeclarationCheckOrder      = []string{"type", "authority-id", "series"}
	builtinBaseDeclarationExpectedHeaders = map[string]any{
		"type":         "base-declaration",
		"authority-id": "canonical",
		"series":       release.Series,
	}
)

// InitBuiltinBaseDeclaration initializes the builtin base-declaration based on headers (or resets it if headers is nil).
func InitBuiltinBaseDeclaration(headers []byte) error {
	if headers == nil {
		builtinBaseDeclaration = nil
		return nil
	}
	trimmed := bytes.TrimSpace(headers)
	h, err := parseHeaders(trimmed)
	if err != nil {
		return err
	}
	for _, name := range builtinBaseDeclarationCheckOrder {
		expected := builtinBaseDeclarationExpectedHeaders[name]
		if h[name] != expected {
			return fmt.Errorf("the builtin base-declaration %q header is not set to expected value %q", name, expected)
		}
	}
	revision, err := checkRevision(h)
	if err != nil {
		return fmt.Errorf("cannot assemble the builtin-base declaration: %v", err)
	}
	h["timestamp"] = time.Now().UTC().Format(time.RFC3339)
	a, err := assembleBaseDeclaration(assertionBase{
		headers:   h,
		body:      nil,
		revision:  revision,
		content:   trimmed,
		signature: []byte("$builtin"),
	})
	if err != nil {
		return fmt.Errorf("cannot assemble the builtin base-declaration: %v", err)
	}
	builtinBaseDeclaration = a.(*BaseDeclaration)
	return nil
}

type dateRange struct {
	Since time.Time
	Until time.Time
}

// SnapDeveloper holds a snap-developer assertion, defining the developers who
// can collaborate on a snap while it's owned by a specific publisher.
//
// The primary key (snap-id, publisher-id) allows a snap to have many
// snap-developer assertions, e.g. to allow a future publisher's collaborations
// to be defined before the snap is transferred. However only the
// snap-developer for the current publisher (the snap-declaration publisher-id)
// is relevant to a device.
type SnapDeveloper struct {
	assertionBase
	developerRanges map[string][]*dateRange
}

// SnapID returns the snap id of the snap.
func (snapdev *SnapDeveloper) SnapID() string {
	return snapdev.HeaderString("snap-id")
}

// PublisherID returns the publisher's account id.
func (snapdev *SnapDeveloper) PublisherID() string {
	return snapdev.HeaderString("publisher-id")
}

func (snapdev *SnapDeveloper) checkConsistency(db RODatabase, acck *AccountKey) error {
	// Check authority is the publisher or trusted.
	authorityID := snapdev.AuthorityID()
	publisherID := snapdev.PublisherID()
	if !db.IsTrustedAccount(authorityID) && (publisherID != authorityID) {
		return fmt.Errorf("snap-developer must be signed by the publisher or a trusted authority but got authority %q and publisher %q", authorityID, publisherID)
	}

	// Check snap-declaration for the snap-id exists for the series.
	// Note: the current publisher is irrelevant here because this assertion
	// may be for a future publisher.
	_, err := db.Find(SnapDeclarationType, map[string]string{
		// XXX: mediate getting current series through some context object? this gets the job done for now
		"series":  release.Series,
		"snap-id": snapdev.SnapID(),
	})
	if err != nil {
		if errors.Is(err, &NotFoundError{}) {
			return fmt.Errorf("snap-developer assertion for snap id %q does not have a matching snap-declaration assertion", snapdev.SnapID())
		}
		return err
	}

	// check there's an account for the publisher-id
	_, err = db.Find(AccountType, map[string]string{"account-id": publisherID})
	if err != nil {
		if errors.Is(err, &NotFoundError{}) {
			return fmt.Errorf("snap-developer assertion for snap-id %q does not have a matching account assertion for the publisher %q", snapdev.SnapID(), publisherID)
		}
		return err
	}

	// check there's an account for each developer
	for developerID := range snapdev.developerRanges {
		if developerID == publisherID {
			continue
		}
		_, err = db.Find(AccountType, map[string]string{"account-id": developerID})
		if err != nil {
			if errors.Is(err, &NotFoundError{}) {
				return fmt.Errorf("snap-developer assertion for snap-id %q does not have a matching account assertion for the developer %q", snapdev.SnapID(), developerID)
			}
			return err
		}
	}

	return nil
}

// expected interface is implemented
var _ consistencyChecker = (*SnapDeveloper)(nil)

// Prerequisites returns references to this snap-developer's prerequisite assertions.
func (snapdev *SnapDeveloper) Prerequisites() []*Ref {
	// Capacity for the snap-declaration, the publisher and all developers.
	refs := make([]*Ref, 0, 2+len(snapdev.developerRanges))

	// snap-declaration
	// XXX: mediate getting current series through some context object? this gets the job done for now
	refs = append(refs, &Ref{SnapDeclarationType, []string{release.Series, snapdev.SnapID()}})

	// the publisher and developers
	publisherID := snapdev.PublisherID()
	refs = append(refs, &Ref{AccountType, []string{publisherID}})
	for developerID := range snapdev.developerRanges {
		if developerID != publisherID {
			refs = append(refs, &Ref{AccountType, []string{developerID}})
		}
	}

	return refs
}

func assembleSnapDeveloper(assert assertionBase) (Assertion, error) {
	developerRanges, err := checkDevelopers(assert.headers)
	if err != nil {
		return nil, err
	}

	return &SnapDeveloper{
		assertionBase:   assert,
		developerRanges: developerRanges,
	}, nil
}

func checkDevelopers(headers map[string]any) (map[string][]*dateRange, error) {
	value, ok := headers["developers"]
	if !ok {
		return nil, nil
	}
	developers, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf(`"developers" must be a list of developer maps`)
	}
	if len(developers) == 0 {
		return nil, nil
	}

	// Used to check for a developer with revoking and non-revoking items.
	// No entry means developer not yet seen, false means seen but not revoked,
	// true means seen and revoked.
	revocationStatus := map[string]bool{}

	developerRanges := make(map[string][]*dateRange)
	for i, item := range developers {
		developer, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf(`"developers" must be a list of developer maps`)
		}

		what := fmt.Sprintf(`in "developers" item %d`, i+1)
		accountID, err := checkStringMatchesWhat(developer, "developer-id", what, validAccountID)
		if err != nil {
			return nil, err
		}

		what = fmt.Sprintf(`in "developers" item %d for developer %q`, i+1, accountID)
		since, err := checkRFC3339DateWhat(developer, "since", what)
		if err != nil {
			return nil, err
		}
		until, err := checkRFC3339DateWithDefaultWhat(developer, "until", what, time.Time{})
		if err != nil {
			return nil, err
		}
		if !until.IsZero() && since.After(until) {
			return nil, fmt.Errorf(`"since" %s must be less than or equal to "until"`, what)
		}

		// Track/check for revocation conflicts.
		revoked := since.Equal(until)
		previouslyRevoked, ok := revocationStatus[accountID]
		if !ok {
			revocationStatus[accountID] = revoked
		} else if previouslyRevoked || revoked {
			return nil, fmt.Errorf(`revocation for developer %q must be standalone but found other "developers" items`, accountID)
		}

		developerRanges[accountID] = append(developerRanges[accountID], &dateRange{since, until})
	}

	return developerRanges, nil
}
