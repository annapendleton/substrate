// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package atomicfile

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// replace moves tmp to path. When path is a regular file it swaps the two
// instead, which leaves the old file at tmp for the caller to remove.
//
// A rename over an existing file makes ext4 start write-out of the new file
// and wait to submit it (auto_da_alloc), so a crash can't leave the file
// empty. Behind a busy disk, such as one writing back a snapshot, that wait
// takes hundreds of milliseconds. Write doesn't promise the contents survive a
// crash, so it swaps instead: an exchange doesn't start write-out.
func replace(tmp, path string) error {
	// Only swap with a regular file: a rename onto anything else fails or
	// replaces it, and an exchange would succeed and move it to tmp.
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return os.Rename(tmp, path)
	}
	err := unix.Renameat2(unix.AT_FDCWD, tmp, unix.AT_FDCWD, path, unix.RENAME_EXCHANGE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.ENOENT), errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS):
		// path was removed meanwhile, or the filesystem can't exchange
		return os.Rename(tmp, path)
	default:
		return &os.LinkError{Op: "renameat2", Old: tmp, New: path, Err: err}
	}
}
