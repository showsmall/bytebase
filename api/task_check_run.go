package api

import (
	"encoding/json"

	"github.com/bytebase/bytebase/common"
	"github.com/bytebase/bytebase/plugin/advisor"
	advisorDB "github.com/bytebase/bytebase/plugin/advisor/db"
	"github.com/bytebase/bytebase/plugin/db"
)

// TaskCheckRunStatus is the status of a task check run.
type TaskCheckRunStatus string

const (
	// TaskCheckRunUnknown is the task check run status for UNKNOWN.
	TaskCheckRunUnknown TaskCheckRunStatus = "UNKNOWN"
	// TaskCheckRunRunning is the task check run status for RUNNING.
	TaskCheckRunRunning TaskCheckRunStatus = "RUNNING"
	// TaskCheckRunDone is the task check run status for DONE.
	TaskCheckRunDone TaskCheckRunStatus = "DONE"
	// TaskCheckRunFailed is the task check run status for FAILED.
	TaskCheckRunFailed TaskCheckRunStatus = "FAILED"
	// TaskCheckRunCanceled is the task check run status for CANCELED.
	TaskCheckRunCanceled TaskCheckRunStatus = "CANCELED"
)

// TaskCheckStatus is the status of a task check.
type TaskCheckStatus string

const (
	// TaskCheckStatusSuccess is the task check status for SUCCESS.
	TaskCheckStatusSuccess TaskCheckStatus = "SUCCESS"
	// TaskCheckStatusWarn is the task check status for WARN.
	TaskCheckStatusWarn TaskCheckStatus = "WARN"
	// TaskCheckStatusError is the task check status for ERROR.
	TaskCheckStatusError TaskCheckStatus = "ERROR"
)

func (t TaskCheckStatus) level() int {
	switch t {
	case TaskCheckStatusSuccess:
		return 2
	case TaskCheckStatusWarn:
		return 1
	case TaskCheckStatusError:
		return 0
	}
	return -1
}

// LessThan helps judge if a task check status doesn't meet the minimum requirement.
// For example, ERROR is LessThan WARN.
func (t TaskCheckStatus) LessThan(r TaskCheckStatus) bool {
	return t.level() < r.level()
}

// TaskCheckType is the type of a taskCheck.
type TaskCheckType string

const (
	// TaskCheckDatabaseStatementFakeAdvise is the task check type for fake advise.
	TaskCheckDatabaseStatementFakeAdvise TaskCheckType = "bb.task-check.database.statement.fake-advise"
	// TaskCheckDatabaseStatementSyntax is the task check type for statement syntax.
	TaskCheckDatabaseStatementSyntax TaskCheckType = "bb.task-check.database.statement.syntax"
	// TaskCheckDatabaseStatementCompatibility is the task check type for statement compatibility.
	TaskCheckDatabaseStatementCompatibility TaskCheckType = "bb.task-check.database.statement.compatibility"
	// TaskCheckDatabaseStatementAdvise is the task check type for schema system review policy.
	TaskCheckDatabaseStatementAdvise TaskCheckType = "bb.task-check.database.statement.advise"
	// TaskCheckDatabaseStatementType is the task check type for statement type.
	TaskCheckDatabaseStatementType TaskCheckType = "bb.task-check.database.statement.type"
	// TaskCheckDatabaseConnect is the task check type for database connection.
	TaskCheckDatabaseConnect TaskCheckType = "bb.task-check.database.connect"
	// TaskCheckInstanceMigrationSchema is the task check type for migrating schemas.
	TaskCheckInstanceMigrationSchema TaskCheckType = "bb.task-check.instance.migration-schema"
	// TaskCheckGhostSync is the task check type for the gh-ost sync task.
	TaskCheckGhostSync TaskCheckType = "bb.task-check.database.ghost.sync"
	// TaskCheckIssueLGTM is the task check type for LGTM comments.
	TaskCheckIssueLGTM TaskCheckType = "bb.task-check.issue.lgtm"
	// TaskCheckPITRMySQL is the task check type for MySQL PITR.
	TaskCheckPITRMySQL TaskCheckType = "bb.task-check.pitr.mysql"
)

// TaskCheckEarliestAllowedTimePayload is the task check payload for earliest allowed time.
type TaskCheckEarliestAllowedTimePayload struct {
	EarliestAllowedTs int64 `json:"earliestAllowedTs,omitempty"`
}

// TaskCheckDatabaseStatementAdvisePayload is the task check payload for database statement advise.
type TaskCheckDatabaseStatementAdvisePayload struct {
	Statement string  `json:"statement,omitempty"`
	DbType    db.Type `json:"dbType,omitempty"`
	Charset   string  `json:"charset,omitempty"`
	Collation string  `json:"collation,omitempty"`

	// SQL review special fields.
	PolicyID int `json:"policyID,omitempty"`
}

// TaskCheckDatabaseStatementTypePayload is the task check payload for SQL type.
type TaskCheckDatabaseStatementTypePayload struct {
	Statement string  `json:"statement,omitempty"`
	DbType    db.Type `json:"dbType,omitempty"`

	// MySQL special fields.
	Charset   string `json:"charset,omitempty"`
	Collation string `json:"collation,omitempty"`
}

// Namespace is the namespace for task check result.
type Namespace string

const (
	// AdvisorNamespace is task check result namespace for advisor.
	AdvisorNamespace Namespace = "bb.advisor"
	// BBNamespace is task check result namespace for bytebase.
	BBNamespace Namespace = "bb.core"
)

// TaskCheckResult is the result of task checks.
type TaskCheckResult struct {
	Namespace Namespace       `json:"namespace,omitempty"`
	Code      int             `json:"code,omitempty"`
	Status    TaskCheckStatus `json:"status,omitempty"`
	Title     string          `json:"title,omitempty"`
	Content   string          `json:"content,omitempty"`
}

// TaskCheckRunResultPayload is the result payload of a task check run.
type TaskCheckRunResultPayload struct {
	Detail     string            `json:"detail,omitempty"`
	ResultList []TaskCheckResult `json:"resultList,omitempty"`
}

// TaskCheckRun is the API message for task check run.
type TaskCheckRun struct {
	ID int `jsonapi:"primary,taskCheckRun"`

	// Standard fields
	CreatorID int
	Creator   *Principal `jsonapi:"relation,creator"`
	CreatedTs int64      `jsonapi:"attr,createdTs"`
	UpdaterID int
	Updater   *Principal `jsonapi:"relation,updater"`
	UpdatedTs int64      `jsonapi:"attr,updatedTs"`

	// Related fields
	TaskID int `jsonapi:"attr,taskId"`

	// Domain specific fields
	Status  TaskCheckRunStatus `jsonapi:"attr,status"`
	Type    TaskCheckType      `jsonapi:"attr,type"`
	Code    common.Code        `jsonapi:"attr,code"`
	Comment string             `jsonapi:"attr,comment"`
	Result  string             `jsonapi:"attr,result"`
	Payload string             `jsonapi:"attr,payload"`
}

// TaskCheckRunCreate is the API message for creating a task check run.
type TaskCheckRunCreate struct {
	// Standard fields
	// Value is assigned from the jwt subject field passed by the client.
	CreatorID int

	// Related fields
	TaskID int

	// Domain specific fields
	Type    TaskCheckType `jsonapi:"attr,type"`
	Comment string        `jsonapi:"attr,comment"`
	Payload string        `jsonapi:"attr,payload"`
}

// TaskCheckRunFind is the API message for finding task check runs.
type TaskCheckRunFind struct {
	ID *int

	// Related fields
	TaskID *int
	Type   *TaskCheckType

	// Domain specific fields
	StatusList *[]TaskCheckRunStatus
	// If true, only returns the latest
	Latest bool
}

func (find *TaskCheckRunFind) String() string {
	str, err := json.Marshal(*find)
	if err != nil {
		return err.Error()
	}
	return string(str)
}

// TaskCheckRunStatusPatch is the API message for patching a task check run.
type TaskCheckRunStatusPatch struct {
	ID *int

	// Standard fields
	UpdaterID int

	// Domain specific fields
	Status TaskCheckRunStatus
	Code   common.Code
	Result string
}

// IsSyntaxCheckSupported checks the engine type if syntax check supports it.
func IsSyntaxCheckSupported(dbType db.Type) bool {
	if dbType == db.Postgres || dbType == db.MySQL || dbType == db.TiDB {
		advisorDB, err := advisorDB.ConvertToAdvisorDBType(string(dbType))
		if err != nil {
			return false
		}

		return advisor.IsSyntaxCheckSupported(advisorDB)
	}

	return false
}

// IsSQLReviewSupported checks the engine type if SQL review supports it.
func IsSQLReviewSupported(dbType db.Type) bool {
	if dbType == db.Postgres || dbType == db.MySQL || dbType == db.TiDB {
		advisorDB, err := advisorDB.ConvertToAdvisorDBType(string(dbType))
		if err != nil {
			return false
		}

		return advisor.IsSQLReviewSupported(advisorDB)
	}

	return false
}

// IsStatementTypeCheckSupported checks the engine type if statement type check supports it.
func IsStatementTypeCheckSupported(dbType db.Type) bool {
	switch dbType {
	case db.Postgres, db.TiDB, db.MySQL:
		return true
	default:
		return false
	}
}
