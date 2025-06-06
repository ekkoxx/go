// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && !osusergo
// +build cgo,!osusergo

package user

import "fmt"

// Not implemented on AIX, see golang.org/issue/30563.

func init() {
	groupListImplemented = false
}

func listGroups(u *User) ([]string, error) {
	return nil, fmt.Errorf("user: list groups for %s: not supported on AIX", u.Username)
}
