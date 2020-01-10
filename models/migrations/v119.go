// Copyright 2020 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package migrations

import (
	"code.gitea.io/gitea/models"
	"xorm.io/xorm"
)

func addUserRepoUnit(x *xorm.Engine) error {

	// AccessMode is Unit's Type
	type AccessMode int

	type UserRepoUnits struct {
		UserID      			int64       `xorm:"pk"`
		RepoID      			int64       `xorm:"pk INDEX"`
		UnitTypeCode            AccessMode
		UnitTypeIssues          AccessMode
		UnitTypePullRequests    AccessMode
		UnitTypeReleases        AccessMode
		UnitTypeWiki            AccessMode
	}

	if err := x.Sync2(new(UserRepoUnits)); err != nil {
		return err
	}

	return models.RebuildAllRepoUnits()
}
