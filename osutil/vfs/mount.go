// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2025 Canonical Ltd
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

package vfs

import (
	"io/fs"
	"strings"
)

// Mount attaches the given file system to given mount point.
//
// Mount attaches a mount entry to the node represented by the mount point. The
// mount point must be an existing directory in the VFS. If the mount point
// contains other mount entries they are shadowed until the new entry is
// unmounted.
func (v *VFS) Mount(fsFS fs.StatFS, mountPoint string) error {
	const op = "mount"

	// Reject paths with trailing slashes. See package description for rationale.
	if strings.HasSuffix(mountPoint, "/") {
		return &fs.PathError{Op: op, Path: mountPoint, Err: errTrailingSlash}
	}

	// Hold lock throughout the function as we need to consistently mutate m.mounts at the end.
	v.mu.Lock()
	defer v.mu.Unlock()

	// Find the mount dominating the mount point.
	pd := v.pathDominator(mountPoint)

	// Stat the mount point through the file system.
	fsFi, err := pd.mount.fsFS.Stat(pd.fsPath)
	if err != nil {
		// Unpack PathError this may have returned as that error contains paths
		// that make sense in the specific [fs.FS] attached to the super-block, but
		// not necessarily in the VFS.
		return &fs.PathError{Op: op, Path: mountPoint, Err: unpackPathError(err)}
	}

	// File systems can only be mounted on existing directories.
	if !fsFi.IsDir() {
		return &fs.PathError{Op: op, Path: mountPoint, Err: errNotDir}
	}

	// Mount and return.
	v.mounts = append(v.mounts, &mount{
		mountID:    v.nextMountID,
		parentID:   pd.mount.mountID,
		mountPoint: mountPoint,
		isDir:      true,
		fsFS:       fsFS,
	})
	v.nextMountID++

	return nil
}

// BindMount creates a mount connecting mountPoint to another location.
//
// BindMount attaches a mount entry to the node represented by source. The
// mount entry points to the node represented by target. Both paths must point
// to existing files or directories. The type of the mount point must match the
// type of the source.
func (v *VFS) BindMount(sourcePoint, mountPoint string) error {
	const op = "bind-mount"

	// Reject paths with trailing slashes. See package description for rationale.
	if strings.HasSuffix(mountPoint, "/") {
		return &fs.PathError{Op: op, Path: mountPoint, Err: errTrailingSlash}
	}

	if strings.HasSuffix(sourcePoint, "/") {
		return &fs.PathError{Op: op, Path: sourcePoint, Err: errTrailingSlash}
	}

	// Hold lock throughout the function as we need to consistently mutate
	// m.mounts at the end.
	v.mu.Lock()
	defer v.mu.Unlock()

	// Find the mount dominating the source point and the mount point.
	sourcePd := v.pathDominator(sourcePoint)
	pd := v.pathDominator(mountPoint)

	// Stat the source point through the file system. The source suffix will
	// influence the rootDir or the new mount entry so keep it.
	sourceFsFi, err := sourcePd.mount.fsFS.Stat(sourcePd.fsPath)
	if err != nil {
		// Unpack PathError this may have returned as that error contains paths
		// that make sense in the specific [fs.FS] attached to the super-block, but
		// not necessarily in the VFS.
		return &fs.PathError{Op: op, Path: sourcePoint, Err: unpackPathError(err)}
	}

	// Stat the mount point through the file system.
	fsFi, err := pd.mount.fsFS.Stat(pd.fsPath)
	if err != nil {
		// Same as above.
		return &fs.PathError{Op: op, Path: mountPoint, Err: unpackPathError(err)}
	}

	// Bind mount must be between two files or two directories.
	if sourceFsFi.IsDir() != fsFi.IsDir() {
		return &fs.PathError{Op: op, Path: mountPoint, Err: fs.ErrInvalid}
	}

	// Mount and return.
	v.mounts = append(v.mounts, &mount{
		mountID:    v.nextMountID,
		parentID:   pd.mount.mountID,
		mountPoint: mountPoint,
		rootDir:    sourcePd.combinedRootDir(),
		isDir:      fsFi.IsDir(),
		fsFS:       sourcePd.mount.fsFS,
	})

	v.nextMountID++

	return nil
}

// Unmount removes a mount attached to a node otherwise named by mountPoint.
//
// Unmount detaches the topmost mount from the node represented by the mount
// point path. The leaf entry is resolved without following mount entires.
func (v *VFS) Unmount(mountPoint string) error {
	const op = "unmount"

	// Reject paths with trailing slashes. See package description for rationale.
	if strings.HasSuffix(mountPoint, "/") {
		return &fs.PathError{Op: op, Path: mountPoint, Err: errTrailingSlash}
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	// Find the mount dominating the mount point. Keep track of the index as we
	// may use it to reslice the mounts array.
	pd := v.pathDominator(mountPoint)

	// If the VFS suffix is not empty then the given path is _not_ a mount point.
	if pd.suffix != "" {
		return &fs.PathError{Op: op, Path: mountPoint, Err: errNotMounted}
	}

	// Is this mount point a parent of any other mount? By special case of the
	// rootfs mount, it cannot ever be unmounted as it is its own parent.
	for _, m := range v.mounts {
		if m.parentID == pd.mount.mountID {
			return &fs.PathError{Op: op, Path: mountPoint, Err: errMountBusy}
		}
	}

	// TODO: use slices from future go to avoid this hand-crafted surgery.
	// Actually forget the mount.
	v.mounts = append(v.mounts[:pd.index], v.mounts[pd.index+1:]...)

	return nil
}
