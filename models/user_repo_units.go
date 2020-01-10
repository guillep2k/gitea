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
)

// UserRepoUnits is an explosion (cartesian product) of all user permissions
// on all repositories, with one record for each combination of user+repo
// except for public repos. General permissions for public repos shared among
// all users (e.g. UnitTypeCode:AccessModeRead) are set for UserID = UserRepoUnitsAnyUser
// in order to reduce the number of records on the table. Special permissions on public
// repos (e.g. writers, owners) are exploded to specific users accordingly.
// This means that to check whether a user has any given permission on a
// particular repository, both UserID == user's and UserID == UserRepoUnitsAnyUser must
// be checked (the highest permission must prevail).

// Except for the special user UserRepoUnitsAnyUser, only real users have records
// in the table (i.e. organizations don't have their own records)

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
	UnitTypeCode           	 AccessMode		`xorm:"NOT NULL DEFAULT 0"`	// 0 = ModeAccessNone
	UnitTypeIssues         	 AccessMode		`xorm:"NOT NULL DEFAULT 0"`	// 0 = ModeAccessNone
	UnitTypePullRequests     AccessMode		`xorm:"NOT NULL DEFAULT 0"`	// 0 = ModeAccessNone
	UnitTypeReleases         AccessMode		`xorm:"NOT NULL DEFAULT 0"`	// 0 = ModeAccessNone
	UnitTypeWiki             AccessMode		`xorm:"NOT NULL DEFAULT 0"`	// 0 = ModeAccessNone
}

// UserRepoUnitsWork is a table to temporarily accumulate all the work performed
// while processing a batch. Ideally, this would be a temporary (no storage) table.
// Records are grouped by BatchID in order to prevent any kind of collision.
type UserRepoUnitsWork struct {
	BatchID					 int64		 	`xorm:"NOT NULL INDEX"`
	UserID      			 int64			`xorm:"NOT NULL"`
	RepoID      			 int64			`xorm:"NOT NULL"`
	UnitTypeCode           	 AccessMode		`xorm:"NOT NULL DEFAULT 0"`	// 0 = ModeAccessNone
	UnitTypeIssues         	 AccessMode		`xorm:"NOT NULL DEFAULT 0"`	// 0 = ModeAccessNone
	UnitTypePullRequests     AccessMode		`xorm:"NOT NULL DEFAULT 0"`	// 0 = ModeAccessNone
	UnitTypeReleases         AccessMode		`xorm:"NOT NULL DEFAULT 0"`	// 0 = ModeAccessNone
	UnitTypeWiki             AccessMode		`xorm:"NOT NULL DEFAULT 0"`	// 0 = ModeAccessNone
}

// UserRepoUnitsBatchNumber provides unique ID values for the batch number
// in a safe way in case we are in a multi-server environment.
// It's a 63-bit number, so good luck reaching the maximum value
// (300 million years at 1000 requests per second, if you want to know).
type UserRepoUnitsBatchNumber struct {
	ID      				 int64			`xorm:"pk autoincr"`
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
	userRepoUnitColumns 	string	// "unit_code, unit_issues, etc."
	userRepoUnitMaxVal		string	// "max(unit_code), max(unit_issues), etc."
	userRepoUnitNotEmpty	string	// "unit_code <> AccessModeNone OR unit_issues <> AccessModeNone ..."
)

func init() {
	var cols, maxs, notempty []string
	for _, col := range unit2Column {
		cols = append(cols, col)
		maxs = append(maxs, fmt.Sprintf("MAX(%s)", col))
		notempty = append(notempty, fmt.Sprintf("%s <> %d", col, AccessModeNone))
	}
	userRepoUnitColumns = strings.Join(cols, ",")
	userRepoUnitMaxVal = strings.Join(maxs, ",")
	userRepoUnitNotEmpty = "(" + strings.Join(notempty, " OR ") + ")"
}

// RebuildAllRemainingRepoUnits will build data for at most maxCount repositories 
func RebuildAllRemainingRepoUnits(maxCount int) (int, error) {

	// Use a single transaction for all the updates
	sess := x.NewSession()
	defer sess.Close()
	if err := sess.Begin(); err != nil {
		return 0, err
	}

	// Since site admins will always have at least code access to all repositories,
	// we can be certain that any repo missing from user_repo_units is unprocessed.
	q := sess.Table("repository").
			  Select("id").
			  Where("NOT EXISTS (SELECT 1 FROM user_repo_units WHERE user_repo_units = repository.id)")
	
	if maxCount > 0 {
		q.Limit(maxCount, 0)
	}

	var repoIDs []int64

	if _, err := q.Get(&repoIDs); err != nil {
		return 0, err
	}

	if len(repoIDs) == 0 {
		return 0, nil
	}

	processed := 0

	for _, repoID := range repoIDs {
		if err := RebuildRepoUnits(sess, repoID); err != nil {
			return 0, err
		}
		processed++
	}

	if err := sess.Commit(); err != nil {
		return 0, err
	}

	return processed, nil
}

// RebuildRepoUnits will rebuild all permissions to a given repository for all users 
func RebuildRepoUnits(e Engine, repoID int64) error {
	batchID, err := userRepoUnitsStartBatch(e)

	repo, err := getRepositoryByID(e, repoID)
	if err != nil {
		return fmt.Errorf("getRepositoryByID(%d): %v", repoID, err)
	}

	if err = repo.getUnits(e); err != nil {
		return fmt.Errorf("getUnits(%d): %v", repoID, err)
	}

	// Make sure we start from scratch
	_, err = e.Delete(&UserRepoUnits{RepoID: repoID})
	if err != nil {
		return fmt.Errorf("DELETE user_repo_units (repoID: %d): %v", repoID, err)
	}

	if err = buildRepoUnits(e, batchID, repo); err != nil {
		return err
	}

	return userRepoUnitsFinishBatch(e, batchID)
}

// buildRepoUnits will build all permissions to a given repository for all users 
func buildRepoUnits(e Engine, batchID int64, repo *Repository) error {

	// Make a list of columns that correspond to the units enabled on the repository
	// UnitTypeCode should always be enabled on a repository, so we assume it is
	// to avoid adding extra checks in the code.
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

	// List of columns to update (unit_code, unit_issues, ... etc)
	cols := strings.Join(slcols,",")[1:]

	// ****************************************************************************
	// Insert permissions for site admins
	// ****************************************************************************

	// Values for the columns (repeats 4, 4, 4 ... etc.)
	vals := strings.Repeat(fmt.Sprintf(",%d", AccessModeAdmin), len(cols))[1:]

	_, err := e.Exec("INSERT INTO user_repo_units_work (batch_id, user_id, repo_id, " + cols +") " +
					"SELECT ?, `user`.id, ?, " + vals + " " +
					"FROM `user` " +
					"WHERE `user`.is_admin = ? AND `user`.is_active = ? AND `user`.prohibit_login = ? " +
					"AND `user`.is_organization = ?",
					batchID, repo.ID, true, true, false, false)
	if err != nil {
		return fmt.Errorf("INSERT INTO user_repo_units_work FROM SELECT (admins): %v", err)
	}

	if err = repo.getOwner(e); err != nil {
		log.Error("Error repository %d has no owner: %v", err)
		// Since the repository has no owner, nobody besides the admins should have permissions
		return batchConsolidateWorkData(e, batchID)
	}

	if repo.Owner.IsOrganization() {

		// ****************************************************************************
		// Insert permissions for the members of teams who have access to this repo
		// ****************************************************************************

		// Process one unit type at a time to simplify SQL code
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
							"AND `user`.is_active = ? AND `user`.prohibit_login = ? AND `user`.is_organization = ? " +
							"AND team.org_id = ?",	// Sanity check, just in case
							batchID, repo.ID, ut, false, true, false, false, repo.OwnerID)
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
							"WHERE team.org_id = ? AND team.includes_all_repositories = ? " +
							"AND team_unit.type = ? " +
							"AND `user`.is_active = ? AND `user`.prohibit_login = ? AND `user`.is_organization = ?",
							batchID, repo.ID, repo.OwnerID, true, ut, true, false, false)
			if err != nil {
				return fmt.Errorf("INSERT INTO user_repo_units_work (teams, include_all = true): %v", err)
			}
		}

	} else if repo.Owner.IsActive && !repo.Owner.ProhibitLogin {

		// ****************************************************************************
		// Insert permissions for the owner (if not inhibited)
		// ****************************************************************************

		vals := strings.Repeat(fmt.Sprintf(",%d", AccessModeOwner), len(cols))[1:]
		_, err = e.Exec("INSERT INTO user_repo_units_work (batch_id, user_id, repo_id, " + cols +") " +
						"VALUES (?, ?, ?, " + vals + ")",
						batchID, repo.OwnerID, repo.ID)
		if err != nil {
			return fmt.Errorf("INSERT INTO user_repo_units_work (owner): %v", err)
		}
	}

	// ****************************************************************************
	// Insert permissions for collaborators
	// ****************************************************************************

	vals = strings.Repeat(",collaboration.mode", len(cols))[1:]
	_, err = e.Exec("INSERT INTO user_repo_units_work (batch_id, user_id, repo_id, " + cols + ") " +
					"SELECT ?, `user`.id, collaboration.repo_id, " + vals + " " +
					"FROM collaboration " +
					"INNER JOIN `user` ON `user`.id = collaboration.user_id " +
					"WHERE collaboration.repo_id = ? " +
					"AND `user`.is_active = ? AND `user`.prohibit_login = ? AND `user`.is_organization = ?",
					batchID, repo.ID, true, false, false)
	if err != nil {
		return fmt.Errorf("INSERT INTO user_repo_units_work (collaboration): %v", err)
	}
	
	if !repo.IsPrivate {

		// ****************************************************************************
		// Process repositories not marked as 'private'
		// ****************************************************************************

		// Public repositories give read access for everybody, but actual visibility
		// depends on whether the repository owner is visible as well.

		if repo.Owner.Visibility == structs.VisibleTypePrivate {

			if repo.Owner.IsOrganization() {

				// ****************************************************************************
				// Public repository for a hidden organization
				// ****************************************************************************

				// All members of the organization get at least read access to the repository
				vals := strings.Repeat(fmt.Sprintf(",%d", AccessModeRead), len(cols))[1:]
				_, err = e.Exec("INSERT INTO user_repo_units_work (batch_id, user_id, repo_id, " + cols + ") " +
								"SELECT ?, `user`.id, ?, " + vals + " " +
								"FROM `user` " +
								"WHERE `user`.id IN (" +
								"  SELECT team_user.uid " +
								"  FROM team_user " +
								"  INNER JOIN team ON team.org_id = ?) " +
								"AND `user`.is_active = ? AND `user`.prohibit_login = ? AND `user`.is_organization = ?",
								batchID, repo.ID, repo.OwnerID, true, false, false)
				if err != nil {
					return fmt.Errorf("INSERT INTO user_repo_units_work (public to organization): %v", err)
				}

			} else {

				// ****************************************************************************
				// Public repository for a hidden user
				// ****************************************************************************
				
				// Currently, only organizations can have visibility == "private",
				// but we can support that for plain users as well by simply doing nothing
				// (which will prevent creating an "any user" permission for the repository).
			}

		} else {

			// ****************************************************************************
			// Public repository for a visible user or organization
			// ****************************************************************************

			// The special user "any user" gets at least read access to the repository
			vals := strings.Repeat(fmt.Sprintf(",%d", AccessModeRead), len(cols))[1:]
			_, err = e.Exec("INSERT INTO user_repo_units_work (batch_id, user_id, repo_id, " + cols +") " +
							"VALUES (?, ?, ?, " + vals + ")",
							batchID, UserRepoUnitsAnyUser, repo.ID)
			if err != nil {
				return fmt.Errorf("INSERT INTO user_repo_units_work (public): %v", err)
			}
		}
	}

	return batchConsolidateWorkData(e, batchID)
}

// userRepoUnitsStartBatch will return a unique ID for the batch transaction
func userRepoUnitsStartBatch(e Engine) (int64, error) {
	var batchnum UserRepoUnitsBatchNumber
	// e.Insert() will return a new ID for the batch that is unique even among
	// concurrent transactions.
	if _, err := e.Insert(batchnum); err != nil {
		return 0, err
	}
	return batchnum.ID, nil
}

func userRepoUnitsFinishBatch(e Engine, batchID int64) error {
	_, err := e.Delete(&UserRepoUnitsWork{BatchID: batchID})
	if err != nil {
		return err
	}
	_, err = e.Delete(&UserRepoUnitsBatchNumber{ID: batchID})
	return err
}

func batchConsolidateWorkData(e Engine, batchID int64) error {
	// UserRepoUnitsWork may contain multiple records for any single user,
	// for example if the user is both a site admin and the repository owner.
	// This function will combine all records into the best set of permissions for each user
	// and insert them into UserRepoUnits.
	// Empty permissions (where all units are AccessModeNone) are skipped, so users with
	// no permissions get no record in UserRepoUnitsWork.
	if _, err := e.Exec("INSERT INTO user_repo_units ( user_id, repo_id, " + userRepoUnitColumns + ") " +
		"SELECT user_id, repo_id, " + userRepoUnitMaxVal + " " +
		"FROM user_repo_units_work WHERE batch_id = ? AND " + userRepoUnitNotEmpty,
		batchID); err != nil {
		return fmt.Errorf("batchConsolidateWorkData (INSERT): %v", err)
	}
	return nil
}