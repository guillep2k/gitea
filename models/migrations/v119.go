// Copyright 2020 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package migrations

import (
	"fmt"

	"code.gitea.io/gitea/models"
	"xorm.io/xorm"
)

func addUserRepoUnit(x *xorm.Engine) error {

	// Don't get too greedy on the batches
	const repoBatchCount = 20

	// AccessMode is Unit's Type
	type AccessMode int

	type UserRepoUnits struct {
		UserID               int64      `xorm:"pk"`
		RepoID               int64      `xorm:"pk INDEX"`
		UnitTypeCode         AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
		UnitTypeIssues       AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
		UnitTypePullRequests AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
		UnitTypeReleases     AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
		UnitTypeWiki         AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
	}

	type UserRepoUnitsWork struct {
		BatchID              int64      `xorm:"NOT NULL INDEX"`
		UserID               int64      `xorm:"NOT NULL"`
		RepoID               int64      `xorm:"NOT NULL"`
		UnitTypeCode         AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
		UnitTypeIssues       AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
		UnitTypePullRequests AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
		UnitTypeReleases     AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
		UnitTypeWiki         AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
	}

	type UserRepoUnitsBatchNumber struct {
		ID int64 `xorm:"pk autoincr"`
	}

	if err := x.Sync2(new(UserRepoUnits), new(UserRepoUnitsWork), new(UserRepoUnitsBatchNumber)); err != nil {
		return err
	}

	// Create access data for the first time
	for {
		count, err := models.RebuildAllRemainingRepoUnits(repoBatchCount)
		if err != nil {
			return fmt.Errorf("RebuildAllRemainingRepoUnits: %v", err)
		}
		if count == 0 {
			break
		}
	}

	return nil
}
