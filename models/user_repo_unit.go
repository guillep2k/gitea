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

	// UserRepoUnitLoggedInUser is a special user ID used in the UserRepoUnit table
	// for permissions that apply to all logged in users when no specific record
	// (userid + repoid) is present. It's intended for public repositories.
	UserRepoUnitLoggedInUser = int64(-1)

	// UserRepoUnitAnyUser is a special user ID used in the UserRepoUnit table
	// for permissions that apply to all users when no specific record
	// (userid + repoid) is present. It's intended for public repositories.
	UserRepoUnitAnyUser = int64(-2)
)

// UserRepoUnit is an explosion (cartesian product) of all user permissions
// on all repositories, with one record for each combination of user+repo,
// except unespecific permissions on public repos. General permissions for public
// repos shared among all users (e.g. UnitTypeCode:AccessModeRead) are set for
// UserID = UserRepoUnitLoggedInUser and UserID = UserRepoUnitAnyUser
// in order to reduce the number of records on the table. Special permissions on public
// repos (e.g. writers, owners) are exploded to specific users accordingly.
// This means that to check whether a given user has any permissions on a
// particular repository, both UserID == user's and UserID == UserRepoUnitLoggedInUser
// must be checked (the highest permission must prevail).
// Anonymous user permissions (i.e. for users not logged in) must be checked
// with UserID == UserRepoUnitAnyUser.

// Except for the special users UserRepoUnitLoggedInUser and UserRepoUnitAnyUser,
// only real users have records in the table (i.e. organizations don't have their
// own records).

// If all permissions result in AccessModeNone, the whole record is omitted,
// so joining against the UserRepoUnit table will result in a quick check for
// whether the user can see the repository at all (e.g. at the explore page).

// Input considered for calculating the records includes:
//
// * public/private status of the repository
// * public/limited/private status of the repository's owner (user or org)
// * user team membership (if repository owner is an org)
// * team settings (whether the team has a repository list or can access all of them)
// * user collaboration status
// * user ownership of the repository
// * user admin status on the system
// * repository units enabled (e.g. issues, PRs, etc.)

// UserRepoUnit is an explosion (cartesian product) of all user permissions
// on all repositories, with one record for each combination of user+repo
type UserRepoUnit struct {
	UserID               int64      `xorm:"pk"`
	RepoID               int64      `xorm:"pk INDEX"`
	UnitTypeCode         AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
	UnitTypeIssues       AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
	UnitTypePullRequests AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
	UnitTypeReleases     AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
	UnitTypeWiki         AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
}

// UserRepoUnitWork is a table used for temporarily accumulate all the work performed
// while processing a batch. Ideally, this would be a temporary (no storage) table.
// Records are grouped by BatchID in order to prevent any kind of collision.
// Lack of primary key is intentional; this table is not intended for replication
// as it should never contain rows after the transaction is completed.
type UserRepoUnitWork struct {
	BatchID              int64      `xorm:"NOT NULL INDEX"`
	UserID               int64      `xorm:"NOT NULL"`
	RepoID               int64      `xorm:"NOT NULL"`
	UnitTypeCode         AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
	UnitTypeIssues       AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
	UnitTypePullRequests AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
	UnitTypeReleases     AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
	UnitTypeWiki         AccessMode `xorm:"NOT NULL DEFAULT 0"` // 0 = ModeAccessNone
}

// UserRepoUnitBatchNumber provides in a safe way unique ID values
// for the batch number in case we are in a multi-server environment.
// It's a 63-bit number, so good luck reaching the maximum value
// (300 million years at 1000 requests per second, if you want to know).
// It's a makeshift replacement for an actual database sequence.
type UserRepoUnitBatchNumber struct {
	ID int64 `xorm:"pk autoincr"`
}

var (
	unit2Column = map[UnitType]string{
		UnitTypeCode:         "unit_type_code",
		UnitTypeIssues:       "unit_type_issues",
		UnitTypePullRequests: "unit_type_pull_requests",
		UnitTypeReleases:     "unit_type_releases",
		UnitTypeWiki:         "unit_type_wiki",
	}

	// Shorthands
	userRepoUnitColumns  string // "unit_code, unit_issues, etc."
	userRepoUnitMaxVal   string // "max(unit_code), max(unit_issues), etc."
	userRepoUnitNotEmpty string // "unit_code <> AccessModeNone OR unit_issues <> AccessModeNone ..."
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

// RebuildPendingRepoUnits will build missing UserRepoUnit data for at most maxCount repositories
func RebuildPendingRepoUnits(maxCount int) (int, error) {

	// Use a single transaction for all the updates
	sess := x.NewSession()
	defer sess.Close()
	if err := sess.Begin(); err != nil {
		return 0, err
	}

	// Since site admins will always have at least code access to all repositories,
	// we can be certain that any repo missing from user_repo_unit requires processing.
	q := sess.Where("NOT EXISTS (SELECT 1 FROM user_repo_unit WHERE user_repo_unit.repo_id = repository.id)")

	if maxCount > 0 {
		q.Limit(maxCount, 0)
	}

	repos := make([]*Repository, 0, 20)

	if err := q.Find(&repos); err != nil {
		return 0, err
	}

	if len(repos) == 0 {
		return 0, nil
	}

	processed := 0

	for _, repo := range repos {
		if err := RebuildRepoUnits(sess, repo); err != nil {
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
func RebuildRepoUnits(e Engine, repo *Repository) error {

	if err := repo.getUnits(e); err != nil {
		return fmt.Errorf("getUnits(%d): %v", repo.ID, err)
	}

	batchID, err := userRepoUnitStartBatch(e)
	if err != nil {
		return fmt.Errorf("userRepoUnitStartBatch: %v", err)
	}

	// Make sure we start from scratch; we intend to recreate all pairs
	_, err = e.Delete(&UserRepoUnit{RepoID: repo.ID})
	if err != nil {
		return fmt.Errorf("DELETE user_repo_unit (repoID: %d): %v", repo.ID, err)
	}

	if err = buildRepoUnits(e, batchID, repo); err != nil {
		return fmt.Errorf("buildRepoUnits(%d): %v", repo.ID, err)
	}

	if err = userRepoUnitsFinishBatch(e, batchID); err != nil {
		return fmt.Errorf("userRepoUnitsFinishBatch(%d): %v", repo.ID, err)
	}

	return nil
}

// buildRepoUnits will build batch data for all users on a given repository
func buildRepoUnits(e Engine, batchID int64, repo *Repository) error {

	// Make a list of columns that correspond to the units enabled on the repository.
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
	cols := strings.Join(slcols, ",")

	// ****************************************************************************
	// Insert permissions for site admins
	// ****************************************************************************

	// Values for the columns (repeats 4, 4, 4 ... etc.)
	vals := strings.Repeat(fmt.Sprintf(",%d", AccessModeAdmin), len(slcols))[1:]

	_, err := e.Exec("INSERT INTO user_repo_unit_work (batch_id, user_id, repo_id, "+cols+") "+
		"SELECT ?, `user`.id, ?, "+vals+" "+
		"FROM `user` "+
		"WHERE `user`.is_admin = ? AND `user`.is_active = ? AND `user`.prohibit_login = ? "+
		"AND `user`.type = ?",
		batchID, repo.ID, true, true, false, UserTypeIndividual)
	if err != nil {
		return fmt.Errorf("INSERT user_repo_unit_work (repo admins): %v", err)
	}

	if err = repo.getOwner(e); err != nil {
		log.Error("Error repository %d has no owner: %v", err)
		// Since the repository has no owner, nobody but the admins should have permissions
		return batchConsolidateWorkData(e, batchID)
	}

	if repo.Owner.IsOrganization() {

		// ****************************************************************************
		// Insert permissions for the members of teams that have access to this repo
		// ****************************************************************************

		// Process one unit type at a time to simplify SQL code
		for _, ut := range slunits {

			// This query will cover all teams with includes_all_repositories = false
			// "Find all users belonging to teams to which this repository is assigned"
			_, err = e.Exec("INSERT INTO user_repo_unit_work (batch_id, user_id, repo_id, "+unit2Column[ut]+") "+
				"SELECT ?, `user`.id, team_repo.repo_id, team.authorize "+
				"FROM team_repo "+
				"INNER JOIN team ON team.id = team_repo.team_id "+
				"INNER JOIN team_unit ON team_unit.team_id = team.id "+
				"INNER JOIN team_user ON team_user.team_id = team.id "+
				"INNER JOIN `user` ON `user`.id = team_user.uid "+
				"WHERE team_repo.repo_id = ? AND team_unit.type = ? AND team.includes_all_repositories = ? "+
				"AND `user`.is_active = ? AND `user`.prohibit_login = ? AND `user`.type = ? "+
				"AND team.org_id = ?", // Sanity check, just in case
				batchID, repo.ID, ut, false, true, false, UserTypeIndividual, repo.OwnerID)
			if err != nil {
				return fmt.Errorf("INSERT user_repo_unit_work (repo teams, include_all = false): %v", err)
			}

			// This query will cover all teams with includes_all_repositories = true
			// "Find all users belonging to teams of the same organization as the repository owner"
			_, err = e.Exec("INSERT INTO user_repo_unit_work (batch_id, user_id, repo_id, "+unit2Column[ut]+") "+
				"SELECT ?, `user`.id, ?, team.authorize "+
				"FROM team "+
				"INNER JOIN team_unit ON team_unit.team_id = team.id "+
				"INNER JOIN team_user ON team_user.team_id = team.id "+
				"INNER JOIN `user` ON `user`.id = team_user.uid "+
				"WHERE team.org_id = ? AND team.includes_all_repositories = ? "+
				"AND team_unit.type = ? "+
				"AND `user`.is_active = ? AND `user`.prohibit_login = ? AND `user`.type = ?",
				batchID, repo.ID, repo.OwnerID, true, ut, true, false, UserTypeIndividual)
			if err != nil {
				return fmt.Errorf("INSERT user_repo_unit_work (repo teams, include_all = true): %v", err)
			}
		}

	} else if repo.Owner.IsActive && !repo.Owner.ProhibitLogin {

		// ****************************************************************************
		// Insert permissions for the owner (if not inhibited)
		// ****************************************************************************

		vals := strings.Repeat(fmt.Sprintf(",%d", AccessModeOwner), len(slcols))[1:]
		_, err = e.Exec("INSERT INTO user_repo_unit_work (batch_id, user_id, repo_id, "+cols+") "+
			"VALUES (?, ?, ?, "+vals+")",
			batchID, repo.OwnerID, repo.ID)
		if err != nil {
			return fmt.Errorf("INSERT user_repo_unit_work (repo owner): %v", err)
		}
	}

	// ****************************************************************************
	// Insert permissions for collaborators
	// ****************************************************************************

	vals = strings.Repeat(",collaboration.mode", len(slcols))[1:]
	_, err = e.Exec("INSERT INTO user_repo_unit_work (batch_id, user_id, repo_id, "+cols+") "+
		"SELECT ?, `user`.id, collaboration.repo_id, "+vals+" "+
		"FROM collaboration "+
		"INNER JOIN `user` ON `user`.id = collaboration.user_id "+
		"WHERE collaboration.repo_id = ? "+
		"AND `user`.is_active = ? AND `user`.prohibit_login = ? AND `user`.type = ?",
		batchID, repo.ID, true, false, UserTypeIndividual)
	if err != nil {
		return fmt.Errorf("INSERT user_repo_unit_work (repo collaborators): %v", err)
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
				vals := strings.Repeat(fmt.Sprintf(",%d", AccessModeRead), len(slcols))[1:]
				_, err = e.Exec("INSERT INTO user_repo_unit_work (batch_id, user_id, repo_id, "+cols+") "+
					"SELECT ?, `user`.id, ?, "+vals+" "+
					"FROM `user` "+
					"WHERE `user`.id IN ("+
					"  SELECT team_user.uid "+
					"  FROM team_user "+
					"  INNER JOIN team ON team.org_id = ?) "+
					"AND `user`.is_active = ? AND `user`.prohibit_login = ? AND `user`.type = ?",
					batchID, repo.ID, repo.OwnerID, true, false, UserTypeIndividual)
				if err != nil {
					return fmt.Errorf("INSERT INTO user_repo_unit_work (repo public for org): %v", err)
				}

			}
			/* } else {		 - empty else (intended) not allowed by linter

				// ****************************************************************************
				// Public repository for a hidden user
				// ****************************************************************************

				// Currently, only organizations can have visibility == "private",
				// but we can support the same for plain users as well by simply doing nothing here
				// (which will prevent the creation of "any user" permissions for the repository).
			} */

		} else {

			// ***********************************************************************************
			// "Public" repository for a visible or limited user or organization (logged in users)
			// ***********************************************************************************

			// Not explicit permissions are read-only
			vals := strings.Repeat(fmt.Sprintf(",%d", AccessModeRead), len(slcols))[1:]

			// Logged in users get a record for themselves; this simplifies the queries for
			// permission verification later.
			// This covers organizations with Visibility == structs.VisibleTypeLimited
			_, err = e.Exec("INSERT INTO user_repo_unit_work (batch_id, user_id, repo_id, "+cols+") "+
				"VALUES (?, ?, ?, "+vals+")",
				batchID, UserRepoUnitLoggedInUser, repo.ID)
			if err != nil {
				return fmt.Errorf("INSERT INTO user_repo_unit_work (repo public, logged in): %v", err)
			}

			if repo.Owner.Visibility == structs.VisibleTypePublic {

				// *******************************************************************************
				// "Public" repository for a fully-visible user or organization (anonymous users)
				// *******************************************************************************

				// Records for users that are not logged in.
				// Whether the site requires all users to be logged in to access the data
				// must be considered separately.
				_, err = e.Exec("INSERT INTO user_repo_unit_work (batch_id, user_id, repo_id, "+cols+") "+
					"VALUES (?, ?, ?, "+vals+")",
					batchID, UserRepoUnitAnyUser, repo.ID)
				if err != nil {
					return fmt.Errorf("INSERT INTO user_repo_unit_work (repo public, anonymous): %v", err)
				}
			}
		}
	}

	return batchConsolidateWorkData(e, batchID)
}

// RebuildUserUnits will rebuild all permissions for a given user
func RebuildUserUnits(e Engine, user *User) error {

	batchID, err := userRepoUnitStartBatch(e)
	if err != nil {
		return fmt.Errorf("userRepoUnitStartBatch: %v", err)
	}

	// Make sure we start from scratch; we intend to recreate all pairs
	_, err = e.Delete(&UserRepoUnit{UserID: user.ID})
	if err != nil {
		return fmt.Errorf("DELETE user_repo_unit (userID: %d): %v", user.ID, err)
	}

	if err = buildUserUnits(e, batchID, user); err != nil {
		return fmt.Errorf("buildUserUnits(%d): %v", user.ID, err)
	}

	if err = userRepoUnitsFinishBatch(e, batchID); err != nil {
		return fmt.Errorf("userRepoUnitsFinishBatch(%d): %v", user.ID, err)
	}

	return nil
}

// buildUserUnits will build batch data for a given user on all non-public repositories
func buildUserUnits(e Engine, batchID int64, user *User) error {

	if !user.IsActive || user.ProhibitLogin {
		// No permissions for inactive users
		// FIXME: should this check apply to admins as well?
		// Maybe changing the admin password from the command line should reset these flags.
		return nil
	}

	if user.IsOrganization() {
		// Organizations have no permissions themselves; only their members do
		return nil
	}

	// To simplify code, we will first process the user permissions assuming that
	// repositories have all types of units enabled (e.g. issues, wiki, etc.)
	// After the work records are calculated, we fix them by removing any invalid unit types
	// they may have.

	if user.IsAdmin {

		// ****************************************************************************
		// Site admins have permissions on all repositories
		// ****************************************************************************

		// Values for the columns (repeats 4, 4, 4 ... etc.)
		vals := strings.Repeat(fmt.Sprintf(",%d", AccessModeAdmin), len(unit2Column))[1:]

		_, err := e.Exec("INSERT INTO user_repo_unit_work (batch_id, user_id, repo_id, "+userRepoUnitColumns+") "+
			"SELECT ?, ?, repository.id, "+vals+" "+
			"FROM repository",
			batchID, user.ID)
		if err != nil {
			return fmt.Errorf("INSERT INTO user_repo_unit_work FROM SELECT (admin): %v", err)
		}

	} else {

		// ****************************************************************************
		// Normal user, owned repositories
		// ****************************************************************************

		// Values for the columns (repeats 4, 4, 4 ... etc.)
		vals := strings.Repeat(fmt.Sprintf(",%d", AccessModeOwner), len(unit2Column))[1:]

		_, err := e.Exec("INSERT INTO user_repo_unit_work (batch_id, user_id, repo_id, "+userRepoUnitColumns+") "+
			"SELECT ?, ?, repository.id, "+vals+" "+
			"FROM repository "+
			"WHERE repository.OwnerID = ?",
			batchID, user.ID, user.ID)
		if err != nil {
			return fmt.Errorf("INSERT user_repo_unit_work (user admin): %v", err)
		}

		// ****************************************************************************
		// Normal user, collaborations on repositories
		// ****************************************************************************

		// Values for the columns (repeats collaboration.mode, collaboration.mode, ... etc.)
		vals = strings.Repeat(",collaboration.mode", len(unit2Column))[1:]

		_, err = e.Exec("INSERT INTO user_repo_unit_work (batch_id, user_id, repo_id, "+userRepoUnitColumns+") "+
			"SELECT ?, ?, collaboration.repo_id, "+vals+" "+
			"FROM collaboration "+
			"WHERE collaboration.user_id = ?",
			batchID, user.ID, user.ID)
		if err != nil {
			return fmt.Errorf("INSERT user_repo_unit_work (user collaborator): %v", err)
		}

		// ****************************************************************************
		// Normal user, teams they belong to
		// ****************************************************************************

		// Process one unit type at a time to simplify SQL code
		for ut, col := range unit2Column {

			// This query will cover all teams with includes_all_repositories = false the user belongs
			// "Find all repos assigned to teams this user belongs to"
			_, err = e.Exec("INSERT INTO user_repo_unit_work (batch_id, user_id, repo_id, "+col+") "+
				"SELECT ?, team_user.uid, team_repo.repo_id, team.authorize "+
				"FROM team_user "+
				"INNER JOIN team ON team.id = team_user.team_id "+
				"INNER JOIN team_unit ON team_unit.team_id = team.id "+
				"INNER JOIN team_repo ON team_repo.team_id = team.id "+
				"WHERE team_user.uid = ? AND team_unit.type = ? AND team.includes_all_repositories = ?",
				batchID, user.ID, ut, false)
			if err != nil {
				return fmt.Errorf("INSERT user_repo_unit_work (user teams, include_all = false): %v", err)
			}

			// This query will cover all teams with includes_all_repositories = true the user belongs to
			// "Find all repos belonging to organizations this user belongs to"
			_, err = e.Exec("INSERT INTO user_repo_unit_work (batch_id, user_id, repo_id, "+col+") "+
				"SELECT ?, team_user.uid, repository.id, team.authorize "+
				"FROM team_user "+
				"INNER JOIN team ON team.id = team_user.team_id "+
				"INNER JOIN repository ON repository.owner_d = team.org_id "+
				"INNER JOIN team_unit ON team_unit.team_id = team.id "+
				"WHERE team_user.uid = ? AND team_unit.type = ? AND team.includes_all_repositories = ?",
				batchID, user.ID, ut, true)
			if err != nil {
				return fmt.Errorf("INSERT user_repo_unit_work (user teams, include_all = true): %v", err)
			}
		}
	}

	// ****************************************************************************
	// Fix repository units that don't exist (e.g. unit_type_issues is disabled)
	// ****************************************************************************

	// Build a SQL that will set AccessModeNone on columns corresponding to units
	// each repository has not enabled (let admins access code in all cases).

	setdata := make([]string, 0, len(unit2Column))
	for ut, col := range unit2Column {
		if ut != UnitTypeCode || !user.IsAdmin {
			/*
				UPDATE user_repo_unit_work SET
				unit_type_wiki =				-- example unit_type column
					CASE WHEN EXISTS (			-- check if unit is enabled for that repository
						SELECT 1
						FROM repo_unit AS ru5	-- give an alternative name to avoid collisions
						WHERE ru5.repo_id = user_repo_unit_work.repo_id
							AND ru5.type = 5)		-- 5 is UnitTypeWiki
					THEN unit_type_wiki			-- whichever value the column has in that row
					ELSE AccessModeNone			-- reset value to none if no record found
					END
			*/
			setcol := fmt.Sprintf("%s = CASE WHEN EXISTS ("+
				"SELECT 1 FROM repo_unit AS ru%d "+
				"WHERE ru%d.repo_id = user_repo_unit_work.repo_id "+
				"AND ru%d.type = %d) "+
				"THEN %s ELSE %d END", col, ut, ut, ut, ut, col, AccessModeNone)
			setdata = append(setdata, setcol)
		}
	}

	_, err := e.Exec("UPDATE user_repo_unit_work SET "+strings.Join(setdata, ",")+" "+
		"WHERE batch_id = ?", batchID)
	if err != nil {
		return fmt.Errorf("UPDATE user_repo_unit_work (invalid units): %v", err)
	}

	// Make sure no data remains where all access modes are "AccessModeNone"

	// Build a condition that tests for all unit types
	checkdata := make([]string, 0, len(unit2Column))
	for _, col := range unit2Column {
		// "unit_type_wiki = 0"
		checkdata = append(checkdata, fmt.Sprintf("%s = %d", col, AccessModeNone))
	}

	_, err = e.Exec("DELETE FROM user_repo_unit_work "+
		"WHERE batch_id = ? AND "+strings.Join(checkdata, " AND "), batchID)
	if err != nil {
		return fmt.Errorf("DELETE user_repo_unit_work (no-perm entries): %v", err)
	}

	return batchConsolidateWorkData(e, batchID)
}

// userRepoUnitStartBatch will return a unique ID for the batch transaction
func userRepoUnitStartBatch(e Engine) (int64, error) {
	var batchnum UserRepoUnitBatchNumber
	// e.Insert() will return a new ID for the batch that is unique even among
	// concurrent transactions.
	if _, err := e.Insert(&batchnum); err != nil {
		return 0, err
	}
	if batchnum.ID == 0 {
		return 0, fmt.Errorf("userRepoUnitStartBatch: unable to obtain a proper batch ID")
	}
	return batchnum.ID, nil
}

func userRepoUnitsFinishBatch(e Engine, batchID int64) error {
	_, err := e.Delete(&UserRepoUnitWork{BatchID: batchID})
	if err != nil {
		return err
	}
	_, err = e.Delete(&UserRepoUnitBatchNumber{ID: batchID})
	return err
}

func batchConsolidateWorkData(e Engine, batchID int64) error {
	// UserRepoUnitWork may contain multiple records for any single user,
	// for example if the user is both a site admin and the repository owner.
	// This function will combine all records into the best set of permissions for each user
	// and insert them into UserRepoUnit.
	// Empty permissions (where all units are AccessModeNone) are skipped, so users with
	// no permissions get no record in UserRepoUnitWork.
	if _, err := e.Exec("INSERT INTO user_repo_unit ( user_id, repo_id, "+userRepoUnitColumns+") "+
		"SELECT user_id, repo_id, "+userRepoUnitMaxVal+" "+
		"FROM user_repo_unit_work WHERE batch_id = ? AND "+userRepoUnitNotEmpty+" "+
		"GROUP BY user_id, repo_id",
		batchID); err != nil {
		return fmt.Errorf("batchConsolidateWorkData (INSERT): %v", err)
	}
	return nil
}
