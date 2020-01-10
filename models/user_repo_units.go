// Copyright 2020 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package models

import (
	"fmt"
	"strings"

	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/structs"
)

const (

	// UserRepoUnitsAnyUser is a special user ID used in the UserRepoUnits table
	// for permissions that apply to all users when no specific record
	// (userid + repoid) is present. It's intended for public repositories.
	UserRepoUnitsAnyUser =			int64(-1)

	// UserRepoUnitsRepository is a special user ID used in the UserRepoUnits table
	// to keep a snapshot of the currently enabled repository units.
	UserRepoUnitsRepository =		int64(-2)

	// How many repositories to load at a time in a batch
	repoBatchSize =					100

	// How many users to load at a time in a batch
	userBatchSize =					100
)

// UserRepoUnits is an explosion (cartesian product) of all user permissions
// on all repositories, with one record for each combination of user+repo
// except for public repos. General permissions for public repos shared among
// all users (e.g. UnitTypeCode:AccessModeRead) are set for UserID = UserRepoUnitsAnyUser
// in order to reduce the number of records on the table. Special permissions on public
// repos (e.g. writers, owners) are exploded to specific users accordingly.
// This means that for checking whether a user has any given permission on a
// particular repository, both UserID == user's and UserID == UserRepoUnitsAnyUser must
// be checked (the higher permission prevails).

// Except for the special user UserRepoUnitsAnyUser, only real users have records
// in the table (i.e. organizations don't have their own record)

// If all permissions result in AccessModeNone, the record is omited, so joining
// against this table will result in a quick check for whether the user can
// see the repository at all (e.g. the explore page).

// Input considered for calculating the records includes:
//
// * public status of the repository
// * public status of the repository's owner (user or org)
// * user team membership (if owner is an org)
// * user collaborator status
// * user ownership on the repository
// * user admin status on the repository
// * user admin status on the system
// * repository units enabled (e.g. issues, PRs, etc.)

// UserRepoUnits is an explosion (cartesian product) of all user permissions
// on all repositories, with one record for each combination of user+repo
type UserRepoUnits struct {
	UserID      			 int64      	`xorm:"pk"`
	RepoID      			 int64      	`xorm:"pk INDEX"`
	UnitTypeCode           	 AccessMode		`xorm:"NOT NULL DEFAULT 0"`
	UnitTypeIssues         	 AccessMode		`xorm:"NOT NULL DEFAULT 0"`
	UnitTypePullRequests     AccessMode		`xorm:"NOT NULL DEFAULT 0"`
	UnitTypeReleases         AccessMode		`xorm:"NOT NULL DEFAULT 0"`
	UnitTypeWiki             AccessMode		`xorm:"NOT NULL DEFAULT 0"`
}

// UserRepoUnitsWork is a table to temporarily accumulate all the work performed
// while processing a batch. Ideally, this would be a temporary (no storage) table.
type UserRepoUnitsWork struct {
	BatchID					 int64		 	`xorm:"NOT NULL INDEX"`
	UserID      			 int64			`xorm:"NOT NULL"`
	RepoID      			 int64			`xorm:"NOT NULL"`
	UnitTypeCode           	 AccessMode		`xorm:"NOT NULL DEFAULT 0"`
	UnitTypeIssues         	 AccessMode		`xorm:"NOT NULL DEFAULT 0"`
	UnitTypePullRequests     AccessMode		`xorm:"NOT NULL DEFAULT 0"`
	UnitTypeReleases         AccessMode		`xorm:"NOT NULL DEFAULT 0"`
	UnitTypeWiki             AccessMode		`xorm:"NOT NULL DEFAULT 0"`
}

var (
	unit2Column = map[UnitType]string {
		UnitTypeCode:            "unit_code",
		UnitTypeIssues:          "unit_issues",
		UnitTypePullRequests:    "unit_pull_requests",
		UnitTypeReleases:        "unit_releases",
		UnitTypeWiki:            "unit_wiki",
	}

	// Shorthands
	userRepoUnitColumns string	// "unit_code, unit_issues, etc."
	userRepoUnitMaxVal	string	// "max(unit_code), max(unit_issues), etc."
	userRepoUnitArgs	string	// "?, ?, ?, ?, ?"
)

// UserRepoUnitsBatchType specifies the type of batch to process
type UserRepoUnitsBatchType int

const (
	// UserRepoUnitsBatchOwner recalculate repository owner
	// Any previous owner is deleted and the new owner is added
	// Used for new repositories or for transferring ownership
	UserRepoUnitsBatchOwner UserRepoUnitsBatchType = iota	// 0

	// UserRepoUnitsBatchRepoTeam recalculate a team's access for an organization's repository
	// Used when a repository is added/removed from a team
	// It assumes the members list has not changed
	UserRepoUnitsBatchRepoTeam								// 1

	// UserRepoUnitsBatchRepoCollaborator recalculate collaborator access for repository
	// Used when a user is added as collaborator, or their access level is modified
	UserRepoUnitsBatchRepoCollaborator						// 2

	// UserRepoUnitsBatchRepoUser recalculate a user access for a repository
	// Used when other batch types wouldn't cover the change (e.g. user removed as collaborator)
	UserRepoUnitsBatchRepoUser								// 3

	// UserRepoUnitsBatchTeam recalculate a team's users access for all repositories
	// Used when a team has changed access level (e.g. from reader to writer)
	// It assumes the members list has not changed
	UserRepoUnitsBatchTeam									// 4

	// UserRepoUnitsBatchUser recalculate a user access for all repositories
	// Used when other batch types wouldn't cover the change (e.g. user was disabled)
	UserRepoUnitsBatchUser									// 5

	// UserRepoUnitsBatchAnyUser recalculate user access for "any user" for repository
	// Used when a repository changed it's visibility status (i.e. is_public)
	UserRepoUnitsBatchAnyUser								// 6

	// UserRepoUnitsBatchUnits recalculate units for repository (issues, prs, wiki, etc.)
	// Used when a repository added or removed some unit (e.g. wiki)
	UserRepoUnitsBatchUnits 								// 7
)

// UserRepoUnitsBatch is a batch of changes to be processed into UserRepoUnits.
type UserRepoUnitsBatch struct {
	ID              int64       `xorm:"pk autoincr"`

	BatchType		UserRepoUnitsBatchType
	RepoID			int64
	UserID			int64
	TeamID			int64
	CollaboratorID	int64
	FromScratch		bool		// No need to check for outdated entries if true
}

// RebuildAllRepoUnits will trigger a batch of rebuild for all repositories.
func RebuildAllRepoUnits() error {

	sess := x.NewSession()
	defer sess.Close()
	if err := sess.Begin(); err != nil {
		return err
	}

	// Remove any pending batches, since all repositories will be processed again
	if _, err := sess.Exec("DELETE FROM user_repo_units"); err != nil {
		return fmt.Errorf("DELETE user_repo_units: %v", err)
	}

	var (
		maxRepoID, maxUserID	int64	
	)

	// Get last values for repository and user; we will recalculate in batches
	// on a per-user basis (because teams are more efficiently checked this way)
	if _, err := sess.Table("repository").Select("MAX(`id`)").Get(&maxRepoID); err != nil {
		return fmt.Errorf("Finding MAX(RepoID): %v", err)
	}

	if _, err := sess.Table("`user`").Select("MAX(`id`)").Get(&maxUserID); err != nil {
		return fmt.Errorf("Finding MAX(UserID): %v", err)
	}

	for repoStart := int64(1) ; repoStart <= maxRepoID ; repoStart += repoBatchSize {
		for userStart := int64(1) ; userStart <= maxUserID ; userStart += repoBatchSize {

		}
	}

	return sess.Commit()
}

func rebuildRepoUnitsBatch(e Engine) error {
	// The fastest would be a TRUNCATE TABLE, but that is DML and would execute
	// outside any transaction, breaking readers.
	if _, err := e.Exec("DELETE FROM user_repo_units"); err != nil {
		return fmt.Errorf("DELETE user_repo_units: %v", err)
	}

	var (
		maxRepoID, maxUserID	int64	
	)

	// Get last values for repository and user; we will 
	if _, err := e.Table("repository").Select("MAX(`id`)").Get(&maxRepoID); err != nil {
		return fmt.Errorf("Finding MAX(RepoID): %v", err)
	}

	if _, err := e.Table("`user`").Select("MAX(`id`)").Get(&maxUserID); err != nil {
		return fmt.Errorf("Finding MAX(UserID): %v", err)
	}

	for repoStart := int64(1) ; repoStart <= maxRepoID ; repoStart += repoBatchSize {

	}

	return nil
}

// rebuildRepoUnits will rebuild all permissions to a given repository for all users 
func rebuildRepoUnits(e Engine, batchID, repoID int64) error {

	repo, err := getRepositoryByID(e, repoID)
	if err != nil {
		return fmt.Errorf("getRepositoryByID(%d): %v", repoID, err)
	}

	if err = repo.getUnits(e); err != nil {
		return fmt.Errorf("getUnits(%d): %v", repoID, err)
	}

	// Start from scratch
	_, err = e.Delete(&UserRepoUnits{RepoID: repoID})
	if err != nil {
		return fmt.Errorf("DELETE user_repo_units (repoID: %d): %v", repoID, err)
	}

	// Build a list units enabled on the repository
	// UnitTypeCode should always be enabled on a repository, so we assume it is.
	// Build a list columns that correspond to the units enabled on the repository
	slcols := make([]string, 1, len(unit2Column)+1)
	slunits := make([]UnitType, 1, len(unit2Column)+1)
	slcols[0] = unit2Column[UnitTypeCode]
	slunits[0] = UnitTypeCode
	for _, ru := range repo.Units {
		if ru.Type != UnitTypeCode {
			if col, ok := unit2Column[ru.Type]; ok {
				slcols = append(slcols, col)
				slunits = append(slunits, ru.Type)
			}
		}
	}

	// Columns to update (unit_code, unit_issues, ... etc)
	cols := strings.Join(slcols,",")[1:]

	// Values for the columns (repeats 4, 4, 4 ... etc.)
	vals := strings.Repeat(fmt.Sprintf(",%d", AccessModeAdmin), len(cols))[1:]

	// Insert permissions for site admins
	_, err = e.Exec("INSERT INTO user_repo_units_work (batch_id, user_id, repo_id, " + cols +") " +
					"SELECT ?, `user`.id, ?, " + vals + " " +
					"FROM `user` " +
					"WHERE `user`.is_admin = ? AND `user`.is_active = ? AND `user`.prohibit_login = ?",
					batchID, repoID, true, true, false)
	if err != nil {
		return fmt.Errorf("INSERT INTO user_repo_units_work FROM SELECT (admins): %v", err)
	}

	if err = repo.getOwner(e); err != nil {
		log.Error("Error repository %d has no owner: %v", err)
		// Since the repository has no owner, nobody else has permissions
		return nil
	}

	if repo.Owner.IsOrganization() {
		// Insert permissions for the team members by unit type, one type at a time
		for _, ut := range slunits {
			// This query will cover all teams with includes_all_repositories = false
			_, err = e.Exec("INSERT INTO user_repo_units_work (batch_id, user_id, repo_id, " + unit2Column[ut] + ") " +
							"SELECT ?, `user`.id, team_repo.repo_id, team.authorize " +
							"FROM team_repo " +
							"INNER JOIN team ON team.id = team_repo.team_id " +
							"INNER JOIN team_unit ON team_unit.team_id = team.id " +
							"INNER JOIN team_user ON team_user.team_id = team.id " +
							"INNER JOIN `user` ON `user`.id = team_user.uid " +
							"WHERE team_repo.repo_id = ? AND team_unit.type = ? AND team.includes_all_repositories = ? " +
							"AND `user`.is_active = ? AND `user`.prohibit_login = ? " +
							"AND team.org_id = ?",	// Sanity check, just in case
							batchID, repoID, ut, false, true, false, repo.OwnerID)
			if err != nil {
				return fmt.Errorf("INSERT INTO user_repo_units_work (teams, include_all = false): %v", err)
			}
			// This query will cover all teams with includes_all_repositories = true
			_, err = e.Exec("INSERT INTO user_repo_units_work (batch_id, user_id, repo_id, " + unit2Column[ut] + ") " +
							"SELECT ?, `user`.id, ?, team.authorize " +
							"FROM team " +
							"INNER JOIN team_unit ON team_unit.team_id = team.id " +
							"INNER JOIN team_user ON team_user.team_id = team.id " +
							"INNER JOIN `user` ON `user`.id = team_user.uid " +
							"WHERE team.org_id = ? "+
							"AND team_unit.type = ? AND team.includes_all_repositories = ? " +
							"AND `user`.is_active = ? AND `user`.prohibit_login = ?",
							batchID, repoID, repo.OwnerID, ut, true, true, false)
			if err != nil {
				return fmt.Errorf("INSERT INTO user_repo_units_work (teams, include_all = true): %v", err)
			}
		}

	} else if repo.Owner.IsActive && !repo.Owner.ProhibitLogin {

		// Insert permissions for the owner
		vals := strings.Repeat(fmt.Sprintf(",%d", AccessModeOwner), len(cols))[1:]
		_, err = e.Exec("INSERT INTO user_repo_units_work (batch_id, user_id, repo_id, " + cols +") " +
						"VALUES (?, ?, ?, " + vals + ")",
						batchID, repo.OwnerID, repoID)
		if err != nil {
			return fmt.Errorf("INSERT INTO user_repo_units_work (owner): %v", err)
		}
	}

	// Insert permissions for collaborators
	vals = strings.Repeat(",collaboration.mode", len(cols))[1:]
	_, err = e.Exec("INSERT INTO user_repo_units_work (batch_id, user_id, repo_id, " + cols + ") " +
					"SELECT ?, `user`.id, collaboration.repo_id, " + vals + " " +
					"FROM collaboration " +
					"INNER JOIN `user` ON `user`.id = collaboration.user_id " +
					"WHERE collaboration.repo_id = ? " +
					"AND `user`.is_active = ? AND `user`.prohibit_login = ?",
					batchID, repoID, true, false)
	if err != nil {
		return fmt.Errorf("INSERT INTO user_repo_units_work (collaboration): %v", err)
	}

	if !repo.IsPrivate {
		// Public repositories give read access for everybody, but visibility is subject to the owner's
		if repo.Owner.Visibility == structs.VisibleTypePrivate {
			if repo.Owner.IsOrganization() {
				// All members of the organization get at least read access to the repository
				vals := strings.Repeat(fmt.Sprintf(",%d", AccessModeRead), len(cols))[1:]
				_, err = e.Exec("INSERT INTO user_repo_units_work (batch_id, user_id, repo_id, " + cols + ") " +
								"SELECT ?, `user`.id, ?, " + vals + " " +
								"FROM `user` " +
								"WHERE `user`.id IN (" +
								"  SELECT team_user.uid " +
								"  FROM team_user " +
								"  INNER JOIN team ON team.org_id = ?) " +
								"AND `user`.is_active = ? AND `user`.prohibit_login = ?",
								batchID, repoID, repo.OwnerID, true, false)
				if err != nil {
					return fmt.Errorf("INSERT INTO user_repo_units_work (public to organization): %v", err)
				}
			} else {
				// Currently, only organizations can have visibility == "private",
				// but we can support it for plain users as well here just by doing nothing
				// (which will prevent creating an "any user" permission for the repository).
			}
		} else {
			// The special user "any user" gets at least read access to the repository
			vals := strings.Repeat(fmt.Sprintf(",%d", AccessModeRead), len(cols))[1:]
			_, err = e.Exec("INSERT INTO user_repo_units_work (batch_id, user_id, repo_id, " + cols +") " +
							"VALUES (?, ?, ?, " + vals + ")",
							batchID, UserRepoUnitsAnyUser, repoID)
			if err != nil {
				return fmt.Errorf("INSERT INTO user_repo_units_work (public): %v", err)
			}
		}
	}

	return nil
}

// batchConsolidateWorkData moves data from UserRepoUnitsWork into UserRepoUnits
func batchConsolidateWorkData(e Engine, batchID int64, deleteOld bool) error {
	// UserRepoUnitsWork may contain multiple records for any single user.
	// For example if the user is both a site admin and the repository owner.
	// This function will insert the best set of permissions for the user into UserRepoUnits.
	if deleteOld {
		// Remove any records in user_repo_units that will be replaced.
		// An IN clause would be ideal, but it's not supported by SQLite3
		if _, err := e.Exec("DELETE FROM user_repo_units WHERE EXISTS " +
							"(SELECT 1 FROM user_repo_units_work WHERE " +
							"user_repo_units_work.user_id = user_repo_units.user_id AND " +
							"user_repo_units_work.repo_id = user_repo_units.repo_id AND " +
							"user_repo_units_work.batch_id = ?)", batchID); err != nil {
			return fmt.Errorf("batchConsolidateWorkData (DELETE): %v", err)
		}
	}

	// Shorthands
	const userRepoUnitColumnsMax = "max(unit_code), max(unit_issues), max(unit_pull_requests), max(unit_releases), max(unit_wiki)"

	if _, err := e.Exec("INSERT INTO user_repo_units " +
		"SELECT user_id, repo_id, " + userRepoUnitColumnsMax + " " +
		"FROM user_repo_units_work WHERE batch_id = ?)", batchID); err != nil {
		return fmt.Errorf("batchConsolidateWorkData (INSERT): %v", err)
	}

	return batchCleanupTempTable(e, batchID)
}

func batchCleanupTempTable(e Engine, batchID int64) error {
	_, err := e.Delete(&UserRepoUnitsWork{BatchID: batchID})
	return err
}
