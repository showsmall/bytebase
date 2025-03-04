package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/labstack/echo/v4"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/bytebase/bytebase/api"
	"github.com/bytebase/bytebase/common"
	"github.com/bytebase/bytebase/common/log"
	"github.com/bytebase/bytebase/plugin/advisor"
	advisorDB "github.com/bytebase/bytebase/plugin/advisor/db"
	"github.com/bytebase/bytebase/plugin/db"
	"github.com/bytebase/bytebase/plugin/vcs"
	"github.com/bytebase/bytebase/plugin/vcs/github"
	"github.com/bytebase/bytebase/plugin/vcs/gitlab"
	"github.com/bytebase/bytebase/server/component/activity"
	"github.com/bytebase/bytebase/server/utils"
)

const (
	// sqlReviewDocs is the URL for SQL review doc.
	sqlReviewDocs = "https://www.bytebase.com/docs/reference/error-code/advisor"

	// issueNameTemplate should be consistent with UI issue names generated from the frontend except for the timestamp.
	// Because we cannot get the correct timezone of the client here.
	// Example: "[db-5] Alter schema".
	issueNameTemplate = "[%s] %s"
)

func (s *Server) registerWebhookRoutes(g *echo.Group) {
	g.POST("/gitlab/:id", func(c echo.Context) error {
		ctx := c.Request().Context()

		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Failed to read webhook request").SetInternal(err)
		}
		var pushEvent gitlab.WebhookPushEvent
		if err := json.Unmarshal(body, &pushEvent); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Malformed push event").SetInternal(err)
		}
		// This shouldn't happen as we only setup webhook to receive push event, just in case.
		if pushEvent.ObjectKind != gitlab.WebhookPush {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Invalid webhook event type, got %s, want push", pushEvent.ObjectKind))
		}
		repositoryID := fmt.Sprintf("%v", pushEvent.Project.ID)

		filter := func(repo *api.Repository) (bool, error) {
			if c.Request().Header.Get("X-Gitlab-Token") != repo.WebhookSecretToken {
				return false, nil
			}

			return s.isWebhookEventBranch(pushEvent.Ref, repo.BranchFilter)
		}
		repositoryList, err := s.filterRepository(ctx, c.Param("id"), repositoryID, filter)
		if err != nil {
			return err
		}
		if len(repositoryList) == 0 {
			log.Debug("Empty handle repo list. Ignore this push event.")
			return c.String(http.StatusOK, "OK")
		}

		baseVCSPushEvent, err := pushEvent.ToVCS()
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to convert GitLab commits").SetInternal(err)
		}

		createdMessages, err := s.processPushEvent(ctx, repositoryList, baseVCSPushEvent)
		if err != nil {
			return err
		}
		return c.String(http.StatusOK, strings.Join(createdMessages, "\n"))
	})

	g.POST("/github/:id", func(c echo.Context) error {
		ctx := c.Request().Context()

		// This shouldn't happen as we only setup webhook to receive push event, just in case.
		eventType := github.WebhookType(c.Request().Header.Get("X-GitHub-Event"))
		// https://docs.github.com/en/developers/webhooks-and-events/webhooks/about-webhooks#ping-event
		// When we create a new webhook, GitHub will send us a simple ping event to let us know we've set up the webhook correctly.
		// We respond to this event so as not to mislead users.
		if eventType == github.WebhookPing {
			return c.String(http.StatusOK, "OK")
		}
		if eventType != github.WebhookPush {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Invalid webhook event type, got %s, want %s", eventType, github.WebhookPush))
		}

		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Failed to read webhook request").SetInternal(err)
		}
		var pushEvent github.WebhookPushEvent
		if err := json.Unmarshal(body, &pushEvent); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Malformed push event").SetInternal(err)
		}
		repositoryID := pushEvent.Repository.FullName

		filter := func(repo *api.Repository) (bool, error) {
			ok, err := validateGitHubWebhookSignature256(c.Request().Header.Get("X-Hub-Signature-256"), repo.WebhookSecretToken, body)
			if err != nil {
				return false, echo.NewHTTPError(http.StatusInternalServerError, "Failed to validate GitHub webhook signature").SetInternal(err)
			}
			if !ok {
				return false, nil
			}

			return s.isWebhookEventBranch(pushEvent.Ref, repo.BranchFilter)
		}
		repositoryList, err := s.filterRepository(ctx, c.Param("id"), repositoryID, filter)
		if err != nil {
			return err
		}
		if len(repositoryList) == 0 {
			log.Debug("Empty handle repo list. Ignore this push event.")
			return c.String(http.StatusOK, "OK")
		}

		baseVCSPushEvent := pushEvent.ToVCS()

		createdMessages, err := s.processPushEvent(ctx, repositoryList, baseVCSPushEvent)
		if err != nil {
			return err
		}
		return c.String(http.StatusOK, strings.Join(createdMessages, "\n"))
	})

	// id is the webhookEndpointID in repository
	// This endpoint is generated and injected into GitHub action & GitLab CI during the VCS setup.
	g.POST("/sql-review/:id", func(c echo.Context) error {
		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Failed to read SQL review request").SetInternal(err)
		}
		log.Debug("SQL review request received for VCS project",
			zap.String("webhook_endpoint_id", c.Param("id")),
			zap.String("request", string(body)),
		)

		var request api.VCSSQLReviewRequest
		if err := json.Unmarshal(body, &request); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Malformed SQL review request").SetInternal(err)
		}

		filter := func(repo *api.Repository) (bool, error) {
			if !repo.EnableSQLReviewCI {
				log.Debug("Skip repository as the SQL review CI is not enabled.",
					zap.Int("repository_id", repo.ID),
					zap.String("repository_external_id", repo.ExternalID),
				)
				return false, nil
			}

			if !strings.HasPrefix(repo.WebURL, request.WebURL) {
				log.Debug("Skip repository as the web URL is not matched.",
					zap.String("request_web_url", request.WebURL),
					zap.String("repo_web_url", repo.WebURL),
				)
				return false, nil
			}

			token := c.Request().Header.Get("X-SQL-Review-Token")
			if token == s.workspaceID && s.profile.Mode == common.ReleaseModeDev {
				// We will use workspace id as token in integration test.
				return true, nil
			}

			return c.Request().Header.Get("X-SQL-Review-Token") == repo.WebhookSecretToken, nil
		}
		ctx := c.Request().Context()
		repositoryList, err := s.filterRepository(ctx, c.Param("id"), request.RepositoryID, filter)
		if err != nil {
			return err
		}
		if len(repositoryList) == 0 {
			log.Debug("Empty handle repo list. Ignore this request.")
			return c.JSON(http.StatusOK, &api.VCSSQLReviewResult{
				Status:  advisor.Success,
				Content: []string{},
			})
		}

		repo := repositoryList[0]
		prFiles, err := vcs.Get(repo.VCS.Type, vcs.ProviderConfig{}).ListPullRequestFile(
			ctx,
			common.OauthContext{
				ClientID:     repo.VCS.ApplicationID,
				ClientSecret: repo.VCS.Secret,
				AccessToken:  repo.AccessToken,
				RefreshToken: repo.RefreshToken,
				Refresher:    utils.RefreshToken(ctx, s.store, repo.WebURL),
			},
			repo.VCS.InstanceURL,
			request.RepositoryID,
			request.PullRequestID,
		)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to list pull request file").SetInternal(err)
		}

		distinctFileList := []vcs.DistinctFileItem{}
		for _, prFile := range prFiles {
			if prFile.IsDeleted {
				continue
			}
			distinctFileList = append(distinctFileList, vcs.DistinctFileItem{
				FileName: prFile.Path,
				Commit: vcs.Commit{
					ID: prFile.LastCommitID,
				},
			})
		}

		sqlCheckAdvice := map[string][]advisor.Advice{}
		var wg sync.WaitGroup

		repoID2FileItemList := groupFileInfoByRepo(distinctFileList, repositoryList)
		for _, fileInfoListInRepo := range repoID2FileItemList {
			for _, file := range fileInfoListInRepo {
				wg.Add(1)
				go func(file fileInfo) {
					defer wg.Done()
					adviceList, err := s.sqlAdviceForFile(ctx, file)
					if err != nil {
						log.Debug(
							"Failed to take SQL review for file",
							zap.String("file", file.item.FileName),
							zap.String("external_id", file.repository.ExternalID),
							zap.Error(err),
						)
					} else if adviceList != nil {
						sqlCheckAdvice[file.item.FileName] = adviceList
					}
				}(file)
			}
		}

		wg.Wait()

		response := &api.VCSSQLReviewResult{}
		switch repo.VCS.Type {
		case vcs.GitHubCom:
			response = convertSQLAdiceToGitHubActionResult(sqlCheckAdvice)
		case vcs.GitLabSelfHost:
			response = convertSQLAdviceToGitLabCIResult(sqlCheckAdvice)
		}

		log.Debug("SQL review finished",
			zap.String("pull_request", request.PullRequestID),
			zap.String("status", string(response.Status)),
			zap.String("content", strings.Join(response.Content, "\n")),
			zap.String("repository_id", request.RepositoryID),
			zap.String("vcs", string(repo.VCS.Type)),
		)

		return c.JSON(http.StatusOK, response)
	})
}

func (s *Server) sqlAdviceForFile(
	ctx context.Context,
	fileInfo fileInfo,
) ([]advisor.Advice, error) {
	log.Debug("Processing file",
		zap.String("file", fileInfo.item.FileName),
		zap.String("vcs", string(fileInfo.repository.VCS.Type)),
	)

	// TODO: support tenant mode project
	if fileInfo.repository.Project.TenantMode == api.TenantModeTenant {
		return []advisor.Advice{
			{
				Status:  advisor.Warn,
				Code:    advisor.Unsupported,
				Title:   "Tenant mode is not supported",
				Content: fmt.Sprintf("Project %s a tenant mode project.", fileInfo.repository.Project.Name),
				Line:    1,
			},
		}, nil
	}

	// TODO(ed): findProjectDatabases doesn't support the tenant mode.
	// We can use https://github.com/bytebase/bytebase/blob/main/server/issue.go#L691 to find databases in tenant mode project.
	databases, err := s.findProjectDatabases(ctx, fileInfo.repository.ProjectID, fileInfo.migrationInfo.Database, fileInfo.migrationInfo.Environment)
	if err != nil {
		log.Debug(
			"Failed to list databse migration info",
			zap.Int("project", fileInfo.repository.ProjectID),
			zap.String("database", fileInfo.migrationInfo.Database),
			zap.String("environment", fileInfo.migrationInfo.Environment),
			zap.Error(err),
		)
		return nil, errors.Errorf("Failed to list databse with error: %v", err)
	}

	fileContent, err := vcs.Get(fileInfo.repository.VCS.Type, vcs.ProviderConfig{}).ReadFileContent(
		ctx,
		common.OauthContext{
			ClientID:     fileInfo.repository.VCS.ApplicationID,
			ClientSecret: fileInfo.repository.VCS.Secret,
			AccessToken:  fileInfo.repository.AccessToken,
			RefreshToken: fileInfo.repository.RefreshToken,
			Refresher:    utils.RefreshToken(ctx, s.store, fileInfo.repository.WebURL),
		},
		fileInfo.repository.VCS.InstanceURL,
		fileInfo.repository.ExternalID,
		fileInfo.item.FileName,
		fileInfo.item.Commit.ID,
	)
	if err != nil {
		return nil, errors.Errorf("Failed to read file cotent for %s with error: %v", fileInfo.item.FileName, err)
	}

	// There may exist many databases that match the file name.
	// We just need to use the first one, which has the SQL review policy and can let us take the check.
	for _, database := range databases {
		environmentResourceType := api.PolicyResourceTypeEnvironment
		policy, err := s.store.GetNormalSQLReviewPolicy(ctx, &api.PolicyFind{ResourceType: &environmentResourceType, ResourceID: &database.Instance.EnvironmentID})
		if err != nil {
			if e, ok := err.(*common.Error); ok && e.Code == common.NotFound {
				log.Debug("Cannot found SQL review policy in environment", zap.Int("Environment", database.Instance.EnvironmentID), zap.Error(err))
				continue
			}

			return nil, errors.Errorf("Failed to get SQL review policy in environment %v with error: %v", database.Instance.EnvironmentID, err)
		}

		dbType, err := advisorDB.ConvertToAdvisorDBType(string(database.Instance.Engine))
		if err != nil {
			return nil, errors.Errorf("Failed to convert database engine type %v to advisor db type with error: %v", database.Instance.Engine, err)
		}

		catalog, err := s.store.NewCatalog(ctx, database.ID, database.Instance.Engine)
		if err != nil {
			return nil, errors.Errorf("Failed to get catalog for database %v with error: %v", database.ID, err)
		}

		driver, err := s.dbFactory.GetReadOnlyDatabaseDriver(ctx, database.Instance, database.Name)
		if err != nil {
			return nil, err
		}
		connection, err := driver.GetDBConnection(ctx, database.Name)
		if err != nil {
			return nil, err
		}

		adviceList, err := advisor.SQLReviewCheck(fileContent, policy.RuleList, advisor.SQLReviewCheckContext{
			Charset:   database.CharacterSet,
			Collation: database.Collation,
			DbType:    dbType,
			Catalog:   catalog,
			Driver:    connection,
			Context:   ctx,
		})
		driver.Close(ctx)
		if err != nil {
			return nil, errors.Errorf("Failed to exec the SQL check for database %v with error: %v", database.ID, err)
		}

		return adviceList, nil
	}

	return []advisor.Advice{
		{
			Status:  advisor.Warn,
			Code:    advisor.NotFound,
			Title:   "SQL review policy not found",
			Content: fmt.Sprintf("You can configure the SQL review policy on %s/setting/sql-review", s.profile.ExternalURL),
			Line:    1,
		},
	}, nil
}

type repositoryFilter func(*api.Repository) (bool, error)

func (s *Server) filterRepository(ctx context.Context, webhookEndpointID string, pushEventRepositoryID string, filter repositoryFilter) ([]*api.Repository, error) {
	repos, err := s.store.FindRepository(ctx, &api.RepositoryFind{WebhookEndpointID: &webhookEndpointID})
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to respond webhook event for endpoint: %v", webhookEndpointID)).SetInternal(err)
	}
	if len(repos) == 0 {
		return nil, echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("Repository for webhook endpoint %s not found", webhookEndpointID))
	}

	var filteredRepos []*api.Repository
	for _, repo := range repos {
		if repo.Project.RowStatus == api.Archived {
			log.Debug("Skip repository as the associated project is archived",
				zap.Int("repository_id", repo.ID),
				zap.String("repository_external_id", repo.ExternalID),
			)
			continue
		}
		if repo.VCS == nil {
			log.Debug("Skipping repo due to missing VCS", zap.Int("repoID", repo.ID))
			continue
		}
		if pushEventRepositoryID != repo.ExternalID {
			log.Debug("Skipping repo due to external ID mismatch", zap.Int("repoID", repo.ID), zap.String("pushEventExternalID", pushEventRepositoryID), zap.String("repoExternalID", repo.ExternalID))
			continue
		}

		ok, err := filter(repo)
		if err != nil {
			return nil, err
		}
		if !ok {
			log.Debug("Skipping repo due to mismatched payload signature", zap.Int("repoID", repo.ID))
			continue
		}

		filteredRepos = append(filteredRepos, repo)
	}
	return filteredRepos, nil
}

func (*Server) isWebhookEventBranch(pushEventRef, branchFilter string) (bool, error) {
	branch, err := parseBranchNameFromRefs(pushEventRef)
	if err != nil {
		return false, echo.NewHTTPError(http.StatusBadRequest, "Invalid ref: %s", pushEventRef).SetInternal(err)
	}
	ok, err := filepath.Match(branchFilter, branch)
	if err != nil {
		return false, errors.Wrapf(err, "failed to match branch filter")
	}
	if !ok {
		log.Debug("Skipping repo due to branch filter mismatch", zap.String("branch", branch), zap.String("filter", branchFilter))
		return false, nil
	}
	return true, nil
}

// validateGitHubWebhookSignature256 returns true if the signature matches the
// HMAC hex digested SHA256 hash of the body using the given key.
func validateGitHubWebhookSignature256(signature, key string, body []byte) (bool, error) {
	signature = strings.TrimPrefix(signature, "sha256=")
	m := hmac.New(sha256.New, []byte(key))
	if _, err := m.Write(body); err != nil {
		return false, err
	}
	got := hex.EncodeToString(m.Sum(nil))

	// NOTE: Use constant time string comparison helps mitigate certain timing
	// attacks against regular equality operators, see
	// https://docs.github.com/en/developers/webhooks-and-events/webhooks/securing-your-webhooks#validating-payloads-from-github
	return subtle.ConstantTimeCompare([]byte(signature), []byte(got)) == 1, nil
}

// parseBranchNameFromRefs parses the branch name from the refs field in the request.
// https://docs.github.com/en/rest/git/refs
// https://docs.gitlab.com/ee/user/project/integrations/webhook_events.html#push-events
func parseBranchNameFromRefs(ref string) (string, error) {
	expectedPrefix := "refs/heads/"
	if !strings.HasPrefix(ref, expectedPrefix) || len(expectedPrefix) == len(ref) {
		log.Debug(
			"ref is not prefix with expected prefix",
			zap.String("ref", ref),
			zap.String("expected prefix", expectedPrefix),
		)
		return ref, errors.Errorf("unexpected ref name %q without prefix %q", ref, expectedPrefix)
	}
	return ref[len(expectedPrefix):], nil
}

func (s *Server) processPushEvent(ctx context.Context, repositoryList []*api.Repository, baseVCSPushEvent vcs.PushEvent) ([]string, error) {
	if len(repositoryList) == 0 {
		return nil, errors.Errorf("empty repository list")
	}

	distinctFileList := baseVCSPushEvent.GetDistinctFileList()
	if len(distinctFileList) == 0 {
		var commitIDs []string
		for _, c := range baseVCSPushEvent.CommitList {
			commitIDs = append(commitIDs, c.ID)
		}
		log.Warn("No files found from the push event",
			zap.String("repoURL", baseVCSPushEvent.RepositoryURL),
			zap.String("repoName", baseVCSPushEvent.RepositoryFullPath),
			zap.String("commits", strings.Join(commitIDs, ",")))
		return nil, nil
	}

	repo := repositoryList[0]
	filteredDistinctFileList, err := s.filterFilesByCommitsDiff(ctx, repo, distinctFileList, baseVCSPushEvent.Before, baseVCSPushEvent.After)
	if err != nil {
		return nil, err
	}

	var createdMessageList []string
	repoID2FileItemList := groupFileInfoByRepo(filteredDistinctFileList, repositoryList)
	for _, fileInfoListInRepo := range repoID2FileItemList {
		// There are possibly multiple files in the push event.
		// Each file corresponds to a (database name, schema version) pair.
		// We want the migration statements are sorted by the file's schema version, and grouped by the database name.
		dbID2FileInfoList := groupFileInfoByDatabase(fileInfoListInRepo)
		for _, fileInfoListInDB := range dbID2FileInfoList {
			fileInfoListSorted := sortFilesBySchemaVersion(fileInfoListInDB)
			repository := fileInfoListSorted[0].repository
			pushEvent := baseVCSPushEvent
			pushEvent.VCSType = repository.VCS.Type
			pushEvent.BaseDirectory = repository.BaseDirectory
			createdMessage, created, activityCreateList, err := s.processFilesInProject(
				ctx,
				pushEvent,
				repository,
				fileInfoListSorted,
			)
			if err != nil {
				return nil, err
			}
			if created {
				createdMessageList = append(createdMessageList, createdMessage)
			} else {
				for _, activityCreate := range activityCreateList {
					if _, err := s.ActivityManager.CreateActivity(ctx, activityCreate, &activity.Metadata{}); err != nil {
						log.Warn("Failed to create project activity for the ignored repository files", zap.Error(err))
					}
				}
			}
		}
	}

	if len(createdMessageList) == 0 {
		var repoURLs []string
		for _, repo := range repositoryList {
			repoURLs = append(repoURLs, repo.WebURL)
		}
		log.Warn("Ignored push event because no applicable file found in the commit list", zap.Strings("repos", repoURLs))
	}

	return createdMessageList, nil
}

// Users may merge commits from other branches, and some of the commits merged in may already be merged into the main branch.
// In that case, the commits in the push event contains files which are not added in this PR/MR.
// We use the compare API to get the file diffs and filter files by the diffs.
// TODO(dragonly): generate distinct file change list from the commits diff instead of filter.
func (s *Server) filterFilesByCommitsDiff(ctx context.Context, repo *api.Repository, distinctFileList []vcs.DistinctFileItem, beforeCommit, afterCommit string) ([]vcs.DistinctFileItem, error) {
	fileDiffList, err := vcs.Get(repo.VCS.Type, vcs.ProviderConfig{}).GetDiffFileList(
		ctx,
		common.OauthContext{
			ClientID:     repo.VCS.ApplicationID,
			ClientSecret: repo.VCS.Secret,
			AccessToken:  repo.AccessToken,
			RefreshToken: repo.RefreshToken,
			Refresher:    utils.RefreshToken(ctx, s.store, repo.WebURL),
		},
		repo.VCS.InstanceURL,
		repo.ExternalID,
		beforeCommit,
		afterCommit,
	)
	if err != nil {
		return nil, errors.WithMessagef(err, "failed to get file diff list for repository %s", repo.ExternalID)
	}
	var filteredDistinctFileList []vcs.DistinctFileItem
	for _, file := range distinctFileList {
		for _, diff := range fileDiffList {
			if file.FileName == diff.Path {
				filteredDistinctFileList = append(filteredDistinctFileList, file)
				break
			}
		}
	}
	return filteredDistinctFileList, nil
}

type fileInfo struct {
	item          vcs.DistinctFileItem
	migrationInfo *db.MigrationInfo
	fType         fileType
	repository    *api.Repository
}

func groupFileInfoByDatabase(fileInfoList []fileInfo) map[string][]fileInfo {
	dbID2FileInfoList := make(map[string][]fileInfo)
	for _, fileInfo := range fileInfoList {
		dbID2FileInfoList[fileInfo.migrationInfo.Database] = append(dbID2FileInfoList[fileInfo.migrationInfo.Database], fileInfo)
	}
	return dbID2FileInfoList
}

// groupFileInfoByRepo groups information for distinct files in the push event by their corresponding api.Repository.
// In a GitLab/GitHub monorepo, a user could create multiple projects and configure different base directory in the repository.
// Bytebase will create a different api.Repository for each project. If the user decides to do a migration in multiple directories at once,
// the push event will trigger changes in multiple projects. So we first group the files into api.Repository, and create issue(s) in
// each project.
func groupFileInfoByRepo(distinctFileList []vcs.DistinctFileItem, repositoryList []*api.Repository) map[int][]fileInfo {
	repoID2FileItemList := make(map[int][]fileInfo)
	for _, item := range distinctFileList {
		log.Debug("Processing file", zap.String("file", item.FileName), zap.String("commit", item.Commit.ID))
		migrationInfo, fType, repository, err := getFileInfo(item, repositoryList)
		if err != nil {
			log.Warn("Failed to get file info for the ignored repository file",
				zap.String("file", item.FileName),
				zap.Error(err),
			)
			continue
		}
		repoID2FileItemList[repository.ID] = append(repoID2FileItemList[repository.ID], fileInfo{
			item:          item,
			migrationInfo: migrationInfo,
			fType:         fType,
			repository:    repository,
		})
	}
	return repoID2FileItemList
}

type fileType int

const (
	unknownFileType fileType = iota
	migrationFileType
	schemaFileType
)

// getFileInfo processes the file item against the candidate list of
// repositories and returns the parsed migration information, file change type
// and a single matched repository. It returns an error when none or multiple
// repositories are matched.
func getFileInfo(fileItem vcs.DistinctFileItem, repositoryList []*api.Repository) (*db.MigrationInfo, fileType, *api.Repository, error) {
	var migrationInfo *db.MigrationInfo
	var fType fileType
	var fileRepositoryList []*api.Repository
	for _, repository := range repositoryList {
		if !strings.HasPrefix(fileItem.FileName, repository.BaseDirectory) {
			log.Debug("Ignored file outside the base directory",
				zap.String("file", fileItem.FileName),
				zap.String("base_directory", repository.BaseDirectory),
			)
			continue
		}

		// NOTE: We do not want to use filepath.Join here because we always need "/" as the path separator.
		filePathTemplate := path.Join(repository.BaseDirectory, repository.FilePathTemplate)
		allowOmitDatabaseName := false
		if repository.Project.TenantMode == api.TenantModeTenant {
			// If the committed file is a YAML file, then the user may have opted-in
			// advanced mode, we need to alter the FilePathTemplate to match ".yml" instead
			// of ".sql".
			if fileItem.IsYAML {
				filePathTemplate = strings.Replace(filePathTemplate, ".sql", ".yml", 1)

				// We do not care database name in the file path with a YAML file.
				allowOmitDatabaseName = true
			} else if repository.Project.DBNameTemplate == "" {
				// Empty DBNameTemplate represents wildcard matching of all databases, thus the
				// database name can be omitted.
				allowOmitDatabaseName = true
			}
		}

		mi, err := db.ParseMigrationInfo(fileItem.FileName, filePathTemplate, allowOmitDatabaseName)
		if err != nil {
			log.Error("Failed to parse migration file info",
				zap.Int("project", repository.ProjectID),
				zap.String("file", fileItem.FileName),
				zap.Error(err),
			)
			continue
		}
		if mi != nil {
			if fileItem.IsYAML && mi.Type != db.Data {
				return nil, unknownFileType, nil, errors.New("only DML is allowed for YAML files in a tenant project")
			}

			migrationInfo = mi
			fType = migrationFileType
			fileRepositoryList = append(fileRepositoryList, repository)
			continue
		}

		si, err := db.ParseSchemaFileInfo(repository.BaseDirectory, repository.SchemaPathTemplate, fileItem.FileName)
		if err != nil {
			log.Debug("Failed to parse schema file info",
				zap.String("file", fileItem.FileName),
				zap.Error(err),
			)
			continue
		}
		if si != nil {
			migrationInfo = si
			fType = schemaFileType
			fileRepositoryList = append(fileRepositoryList, repository)
			continue
		}
	}

	switch len(fileRepositoryList) {
	case 0:
		return nil, unknownFileType, nil, errors.Errorf("file change is not associated with any project")
	case 1:
		return migrationInfo, fType, fileRepositoryList[0], nil
	default:
		var projectList []string
		for _, repository := range fileRepositoryList {
			projectList = append(projectList, repository.Project.Name)
		}
		return nil, unknownFileType, nil, errors.Errorf("file change should be associated with exactly one project but found %s", strings.Join(projectList, ", "))
	}
}

// processFilesInProject attempts to create new issue(s) according to the repository type.
// 1. For a state based project, we create one issue per schema file, and one issue for all of the rest migration files (if any).
// 2. For a migration based project, we create one issue for all of the migration files. All schema files are ignored.
// It returns "created=true" when new issue(s) has been created,
// along with the creation message to be presented in the UI. An *echo.HTTPError
// is returned in case of the error during the process.
func (s *Server) processFilesInProject(ctx context.Context, pushEvent vcs.PushEvent, repo *api.Repository, fileInfoList []fileInfo) (string, bool, []*api.ActivityCreate, *echo.HTTPError) {
	if repo.Project.TenantMode == api.TenantModeTenant && !s.licenseService.IsFeatureEnabled(api.FeatureMultiTenancy) {
		return "", false, nil, echo.NewHTTPError(http.StatusForbidden, api.FeatureMultiTenancy.AccessErrorMessage())
	}

	var migrationDetailList []*api.MigrationDetail
	var activityCreateList []*api.ActivityCreate
	var createdIssueList []string
	var fileNameList []string

	creatorID := s.getIssueCreatorID(ctx, pushEvent.CommitList[0].AuthorEmail)
	for _, fileInfo := range fileInfoList {
		if fileInfo.fType == schemaFileType {
			if repo.Project.SchemaChangeType == api.ProjectSchemaChangeTypeSDL {
				// Create one issue per schema file for SDL project.
				migrationDetailListForFile, activityCreateListForFile := s.prepareIssueFromSDLFile(ctx, repo, pushEvent, fileInfo.migrationInfo, fileInfo.item.FileName)
				activityCreateList = append(activityCreateList, activityCreateListForFile...)
				if len(migrationDetailListForFile) != 0 {
					databaseName := fileInfo.migrationInfo.Database
					issueName := fmt.Sprintf(issueNameTemplate, databaseName, "Alter schema")
					issueDescription := fmt.Sprintf("Apply schema diff by file %s", strings.TrimPrefix(fileInfo.item.FileName, repo.BaseDirectory+"/"))
					if err := s.createIssueFromMigrationDetailList(ctx, issueName, issueDescription, pushEvent, creatorID, repo.ProjectID, migrationDetailListForFile); err != nil {
						return "", false, activityCreateList, echo.NewHTTPError(http.StatusInternalServerError, "Failed to create issue").SetInternal(err)
					}
					createdIssueList = append(createdIssueList, issueName)
				}
			} else {
				log.Debug("Ignored schema file for non-SDL project", zap.String("fileName", fileInfo.item.FileName), zap.String("type", string(fileInfo.item.ItemType)))
			}
		} else { // fileInfo.fType == migrationFileType
			// This is a migration-based DDL or DML file and we would allow it for both DDL and SDL schema change type project.
			// For DDL schema change type project, this is expected.
			// For SDL schema change type project, we allow it because:
			// 1) DML is always migration-based.
			// 2) We may have a limitation in SDL implementation.
			// 3) User just wants to break the glass.
			migrationDetailListForFile, activityCreateListForFile := s.prepareIssueFromFile(ctx, repo, pushEvent, fileInfo)
			activityCreateList = append(activityCreateList, activityCreateListForFile...)
			migrationDetailList = append(migrationDetailList, migrationDetailListForFile...)
			if len(migrationDetailListForFile) != 0 {
				fileNameList = append(fileNameList, strings.TrimPrefix(fileInfo.item.FileName, repo.BaseDirectory+"/"))
			}
		}
	}

	if len(migrationDetailList) == 0 {
		return "", len(createdIssueList) != 0, activityCreateList, nil
	}

	// Create one issue per push event for DDL project, or non-schema files for SDL project.
	migrateType := "Change data"
	for _, d := range migrationDetailList {
		if d.MigrationType == db.Migrate {
			migrateType = "Alter schema"
			break
		}
	}
	// The files are grouped by database names before calling this function, so they have the same database name here.
	databaseName := fileInfoList[0].migrationInfo.Database
	issueName := fmt.Sprintf(issueNameTemplate, databaseName, migrateType)
	issueDescription := fmt.Sprintf("By VCS files:\n\n%s\n", strings.Join(fileNameList, "\n"))
	if err := s.createIssueFromMigrationDetailList(ctx, issueName, issueDescription, pushEvent, creatorID, repo.ProjectID, migrationDetailList); err != nil {
		return "", len(createdIssueList) != 0, activityCreateList, echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to create issue %s", issueName)).SetInternal(err)
	}
	createdIssueList = append(createdIssueList, issueName)

	return fmt.Sprintf("Created issue %q from push event", strings.Join(createdIssueList, ",")), true, activityCreateList, nil
}

func sortFilesBySchemaVersion(fileInfoList []fileInfo) []fileInfo {
	var ret []fileInfo
	ret = append(ret, fileInfoList...)
	sort.Slice(ret, func(i, j int) bool {
		mi := ret[i].migrationInfo
		mj := ret[j].migrationInfo
		return mi.Database < mj.Database || (mi.Database == mj.Database && mi.Version < mj.Version)
	})
	return ret
}

func (s *Server) createIssueFromMigrationDetailList(ctx context.Context, issueName, issueDescription string, pushEvent vcs.PushEvent, creatorID, projectID int, migrationDetailList []*api.MigrationDetail) error {
	createContext, err := json.Marshal(
		&api.MigrationContext{
			VCSPushEvent: &pushEvent,
			DetailList:   migrationDetailList,
		},
	)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "Failed to marshal update schema context").SetInternal(err)
	}

	// TODO(d): unify issue type for database changes.
	issueType := api.IssueDatabaseDataUpdate
	for _, detail := range migrationDetailList {
		if detail.MigrationType == db.Migrate || detail.MigrationType == db.Baseline {
			issueType = api.IssueDatabaseSchemaUpdate
		}
	}
	issueCreate := &api.IssueCreate{
		CreatorID:             creatorID,
		ProjectID:             projectID,
		Name:                  issueName,
		Type:                  issueType,
		Description:           issueDescription,
		AssigneeID:            api.SystemBotID,
		AssigneeNeedAttention: true,
		CreateContext:         string(createContext),
	}
	issue, err := s.createIssue(ctx, issueCreate)
	if err != nil {
		errMsg := "Failed to create schema update issue"
		if issueType == api.IssueDatabaseDataUpdate {
			errMsg = "Failed to create data update issue"
		}
		return echo.NewHTTPError(http.StatusInternalServerError, errMsg).SetInternal(err)
	}

	// Create a project activity after successfully creating the issue from the push event.
	activityPayload, err := json.Marshal(
		api.ActivityProjectRepositoryPushPayload{
			VCSPushEvent: pushEvent,
			IssueID:      issue.ID,
			IssueName:    issue.Name,
		},
	)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "Failed to construct activity payload").SetInternal(err)
	}

	activityCreate := &api.ActivityCreate{
		CreatorID:   creatorID,
		ContainerID: projectID,
		Type:        api.ActivityProjectRepositoryPush,
		Level:       api.ActivityInfo,
		Comment:     fmt.Sprintf("Created issue %q.", issue.Name),
		Payload:     string(activityPayload),
	}
	if _, err := s.ActivityManager.CreateActivity(ctx, activityCreate, &activity.Metadata{}); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to create project activity after creating issue from repository push event: %d", issue.ID)).SetInternal(err)
	}

	return nil
}

func (s *Server) getIssueCreatorID(ctx context.Context, email string) int {
	creatorID := api.SystemBotID
	if email != "" {
		committerPrincipal, err := s.store.GetPrincipalByEmail(ctx, email)
		if err != nil {
			log.Warn("Failed to find the principal with committer email, use system bot instead", zap.String("email", email), zap.Error(err))
		} else if committerPrincipal == nil {
			log.Warn("Principal with committer email does not exist, use system bot instead", zap.String("email", email))
		} else {
			creatorID = committerPrincipal.ID
		}
	}
	return creatorID
}

// findProjectDatabases finds the list of databases with given name in the
// project. If the `envName` is not empty, it will be used as a filter condition
// for the result list.
func (s *Server) findProjectDatabases(ctx context.Context, projectID int, dbName, envName string) ([]*api.Database, error) {
	// Retrieve the current schema from the database
	foundDatabases, err := s.store.FindDatabase(ctx,
		&api.DatabaseFind{
			ProjectID: &projectID,
			Name:      &dbName,
		},
	)
	if err != nil {
		return nil, errors.Wrap(err, "find database")
	} else if len(foundDatabases) == 0 {
		return nil, errors.Errorf("project %d does not have database %q", projectID, dbName)
	}

	// We support 3 patterns on how to organize the schema files.
	// Pattern 1: 	The database name is the same across all environments. Each environment will have its own directory, so the
	//              schema file looks like "dev/v1##db1", "staging/v1##db1".
	//
	// Pattern 2: 	Like 1, the database name is the same across all environments. All environment shares the same schema file,
	//              say v1##db1, when a new file is added like v2##db1##add_column, we will create a multi stage pipeline where
	//              each stage corresponds to an environment.
	//
	// Pattern 3:  	The database name is different among different environments. In such case, the database name alone is enough
	//             	to identify ambiguity.

	// Further filter by environment name if applicable.
	var filteredDatabases []*api.Database
	if envName != "" {
		for _, database := range foundDatabases {
			// Environment name comparison is case insensitive
			if strings.EqualFold(database.Instance.Environment.Name, envName) {
				filteredDatabases = append(filteredDatabases, database)
			}
		}
		if len(filteredDatabases) == 0 {
			return nil, errors.Errorf("project %d does not have database %q for environment %q", projectID, dbName, envName)
		}
	} else {
		filteredDatabases = foundDatabases
	}

	// In case there are databases with identical name in a project for the same environment.
	marked := make(map[int]struct{})
	for _, database := range filteredDatabases {
		if _, ok := marked[database.Instance.EnvironmentID]; ok {
			return nil, errors.Errorf("project %d has multiple databases %q for environment %q", projectID, dbName, envName)
		}
		marked[database.Instance.EnvironmentID] = struct{}{}
	}
	return filteredDatabases, nil
}

// getIgnoredFileActivityCreate get a warning project activityCreate for the ignored file with given error.
func getIgnoredFileActivityCreate(projectID int, pushEvent vcs.PushEvent, file string, err error) *api.ActivityCreate {
	payload, marshalErr := json.Marshal(
		api.ActivityProjectRepositoryPushPayload{
			VCSPushEvent: pushEvent,
		},
	)
	if marshalErr != nil {
		log.Warn("Failed to construct project activity payload for the ignored repository file",
			zap.Error(marshalErr),
		)
		return nil
	}

	return &api.ActivityCreate{
		CreatorID:   api.SystemBotID,
		ContainerID: projectID,
		Type:        api.ActivityProjectRepositoryPush,
		Level:       api.ActivityWarn,
		Comment:     fmt.Sprintf("Ignored file %q, %v.", file, err),
		Payload:     string(payload),
	}
}

// readFileContent reads the content of the given file from the given repository.
func (s *Server) readFileContent(ctx context.Context, pushEvent vcs.PushEvent, repo *api.Repository, file string) (string, error) {
	// Retrieve the latest AccessToken and RefreshToken as the previous
	// ReadFileContent call may have updated the stored token pair. ReadFileContent
	// will fetch and store the new token pair if the existing token pair has
	// expired.
	repos, err := s.store.FindRepository(ctx, &api.RepositoryFind{WebhookEndpointID: &repo.WebhookEndpointID})
	if err != nil {
		return "", errors.Wrapf(err, "get repository by webhook endpoint %q", repo.WebhookEndpointID)
	} else if len(repos) == 0 {
		return "", errors.Wrapf(err, "repository not found by webhook endpoint %q", repo.WebhookEndpointID)
	}

	repo = repos[0]
	content, err := vcs.Get(repo.VCS.Type, vcs.ProviderConfig{}).ReadFileContent(
		ctx,
		common.OauthContext{
			ClientID:     repo.VCS.ApplicationID,
			ClientSecret: repo.VCS.Secret,
			AccessToken:  repo.AccessToken,
			RefreshToken: repo.RefreshToken,
			Refresher:    utils.RefreshToken(ctx, s.store, repo.WebURL),
		},
		repo.VCS.InstanceURL,
		repo.ExternalID,
		file,
		pushEvent.CommitList[len(pushEvent.CommitList)-1].ID,
	)
	if err != nil {
		return "", errors.Wrap(err, "read content")
	}
	return content, nil
}

// prepareIssueFromSDLFile returns the migration info and a list of update
// schema details derived from the given push event for SDL.
func (s *Server) prepareIssueFromSDLFile(ctx context.Context, repo *api.Repository, pushEvent vcs.PushEvent, schemaInfo *db.MigrationInfo, file string) ([]*api.MigrationDetail, []*api.ActivityCreate) {
	dbName := schemaInfo.Database
	if dbName == "" {
		log.Debug("Ignored schema file without a database name", zap.String("file", file))
		return nil, nil
	}

	sdl, err := s.readFileContent(ctx, pushEvent, repo, file)
	if err != nil {
		activityCreate := getIgnoredFileActivityCreate(repo.ProjectID, pushEvent, file, errors.Wrap(err, "Failed to read file content"))
		return nil, []*api.ActivityCreate{activityCreate}
	}

	var migrationDetailList []*api.MigrationDetail
	if repo.Project.TenantMode == api.TenantModeTenant {
		migrationDetailList = append(migrationDetailList,
			&api.MigrationDetail{
				MigrationType: db.MigrateSDL,
				DatabaseName:  dbName,
				Statement:     sdl,
			},
		)
		return migrationDetailList, nil
	}

	envName := schemaInfo.Environment
	databases, err := s.findProjectDatabases(ctx, repo.ProjectID, dbName, envName)
	if err != nil {
		activityCreate := getIgnoredFileActivityCreate(repo.ProjectID, pushEvent, file, errors.Wrap(err, "Failed to find project databases"))
		return nil, []*api.ActivityCreate{activityCreate}
	}

	for _, database := range databases {
		migrationDetailList = append(migrationDetailList,
			&api.MigrationDetail{
				MigrationType: db.MigrateSDL,
				DatabaseID:    database.ID,
				Statement:     sdl,
			},
		)
	}

	return migrationDetailList, nil
}

// prepareIssueFromFile returns a list of update schema details derived
// from the given push event for DDL.
func (s *Server) prepareIssueFromFile(ctx context.Context, repo *api.Repository, pushEvent vcs.PushEvent, fileInfo fileInfo) ([]*api.MigrationDetail, []*api.ActivityCreate) {
	content, err := s.readFileContent(ctx, pushEvent, repo, fileInfo.item.FileName)
	if err != nil {
		return nil, []*api.ActivityCreate{
			getIgnoredFileActivityCreate(
				repo.ProjectID,
				pushEvent,
				fileInfo.item.FileName,
				errors.Wrap(err, "Failed to read file content"),
			),
		}
	}

	if repo.Project.TenantMode == api.TenantModeTenant {
		// A non-YAML file means the whole file content is the SQL statement
		if !fileInfo.item.IsYAML {
			return []*api.MigrationDetail{
				{
					MigrationType: fileInfo.migrationInfo.Type,
					DatabaseName:  fileInfo.migrationInfo.Database,
					Statement:     content,
					SchemaVersion: fileInfo.migrationInfo.Version,
				},
			}, nil
		}

		var migrationFile api.MigrationFileYAML
		err = yaml.Unmarshal([]byte(content), &migrationFile)
		if err != nil {
			return nil, []*api.ActivityCreate{
				getIgnoredFileActivityCreate(
					repo.ProjectID,
					pushEvent,
					fileInfo.item.FileName,
					errors.Wrap(err, "Failed to parse file content as YAML"),
				),
			}
		}

		var migrationDetailList []*api.MigrationDetail
		for _, database := range migrationFile.Databases {
			dbList, err := s.findProjectDatabases(ctx, repo.ProjectID, database.Name, "")
			if err != nil {
				return nil, []*api.ActivityCreate{
					getIgnoredFileActivityCreate(
						repo.ProjectID,
						pushEvent,
						fileInfo.item.FileName,
						errors.Wrapf(err, "Failed to find project database %q", database.Name),
					),
				}
			}

			for _, db := range dbList {
				migrationDetailList = append(migrationDetailList,
					&api.MigrationDetail{
						MigrationType: fileInfo.migrationInfo.Type,
						DatabaseID:    db.ID,
						Statement:     migrationFile.Statement,
						SchemaVersion: fileInfo.migrationInfo.Version,
					},
				)
			}
		}
		return migrationDetailList, nil
	}

	// TODO(dragonly): handle modified file for tenant mode.
	databases, err := s.findProjectDatabases(ctx, repo.ProjectID, fileInfo.migrationInfo.Database, fileInfo.migrationInfo.Environment)
	if err != nil {
		activityCreate := getIgnoredFileActivityCreate(repo.ProjectID, pushEvent, fileInfo.item.FileName, errors.Wrap(err, "Failed to find project databases"))
		return nil, []*api.ActivityCreate{activityCreate}
	}

	if fileInfo.item.ItemType == vcs.FileItemTypeAdded {
		var migrationDetailList []*api.MigrationDetail
		for _, database := range databases {
			migrationDetailList = append(migrationDetailList,
				&api.MigrationDetail{
					MigrationType: fileInfo.migrationInfo.Type,
					DatabaseID:    database.ID,
					Statement:     content,
					SchemaVersion: fileInfo.migrationInfo.Version,
				},
			)
		}
		return migrationDetailList, nil
	}

	if err := s.tryUpdateTasksFromModifiedFile(ctx, databases, fileInfo.item.FileName, fileInfo.migrationInfo.Version, content); err != nil {
		return nil, []*api.ActivityCreate{
			getIgnoredFileActivityCreate(
				repo.ProjectID,
				pushEvent,
				fileInfo.item.FileName,
				errors.Wrap(err, "Failed to find project task"),
			),
		}
	}
	return nil, nil
}

func (s *Server) tryUpdateTasksFromModifiedFile(ctx context.Context, databases []*api.Database, fileName, schemaVersion, statement string) error {
	// For modified files, we try to update the existing issue's statement.
	for _, database := range databases {
		find := &api.TaskFind{
			DatabaseID: &database.ID,
			StatusList: &[]api.TaskStatus{api.TaskPendingApproval, api.TaskFailed},
			TypeList:   &[]api.TaskType{api.TaskDatabaseSchemaUpdate, api.TaskDatabaseDataUpdate},
			Payload:    fmt.Sprintf("payload->>'schemaVersion' = '%s'", schemaVersion),
		}
		taskList, err := s.store.FindTask(ctx, find, true)
		if err != nil {
			return err
		}
		if len(taskList) == 0 {
			continue
		}
		if len(taskList) > 1 {
			log.Error("Found more than one pending approval or failed tasks for modified VCS file, should be only one task.", zap.Int("databaseID", database.ID), zap.String("schemaVersion", schemaVersion))
			return nil
		}
		task := taskList[0]
		taskPatch := api.TaskPatch{
			ID:        task.ID,
			Statement: &statement,
			UpdaterID: api.SystemBotID,
		}
		issue, err := s.store.GetIssueByPipelineID(ctx, task.PipelineID)
		if err != nil {
			log.Error("failed to get issue by pipeline ID", zap.Int("pipeline ID", task.PipelineID), zap.Error(err))
			return nil
		}
		if issue == nil {
			log.Error("issue not found by pipeline ID", zap.Int("pipeline ID", task.PipelineID), zap.Error(err))
			return nil
		}
		// TODO(dragonly): Try to patch the failed migration history record to pending, and the statement to the current modified file content.
		log.Debug("Patching task for modified file VCS push event", zap.String("fileName", fileName), zap.Int("issueID", issue.ID), zap.Int("taskID", task.ID))
		if _, err := s.patchTask(ctx, task, &taskPatch, issue); err != nil {
			log.Error("Failed to patch task with the same migration version", zap.Int("issueID", issue.ID), zap.Int("taskID", task.ID), zap.Error(err))
			return nil
		}
	}
	return nil
}

// convertSQLAdviceToGitLabCIResult will convert SQL advice map to GitLab test output format.
// GitLab test report: https://docs.gitlab.com/ee/ci/testing/unit_test_reports.html
// junit XML format: https://llg.cubic.org/docs/junit/
// nolint:unused
func convertSQLAdviceToGitLabCIResult(adviceMap map[string][]advisor.Advice) *api.VCSSQLReviewResult {
	testsuiteList := []string{}
	status := advisor.Success

	fileList := []string{}
	for filePath := range adviceMap {
		fileList = append(fileList, filePath)
	}
	sort.Strings(fileList)

	for _, filePath := range fileList {
		adviceList := adviceMap[filePath]
		testcaseList := []string{}
		for _, advice := range adviceList {
			if advice.Code == 0 {
				continue
			}

			line := advice.Line
			if line <= 0 {
				line = 1
			}

			if advice.Status == advisor.Error {
				status = advice.Status
			} else if advice.Status == advisor.Warn && status != advisor.Error {
				status = advice.Status
			}

			content := fmt.Sprintf("Error: %s.\nYou can check the docs at %s#%d",
				advice.Content,
				sqlReviewDocs,
				advice.Code,
			)

			testcase := fmt.Sprintf(
				"<testcase name=\"%s\" classname=\"%s\" file=\"%s#L%d\">\n<failure>\n%s\n</failure>\n</testcase>",
				advice.Title,
				filePath,
				filePath,
				line,
				content,
			)

			testcaseList = append(testcaseList, testcase)
		}

		if len(testcaseList) > 0 {
			testsuiteList = append(
				testsuiteList,
				fmt.Sprintf("<testsuite name=\"%s\">\n%s\n</testsuite>", filePath, strings.Join(testcaseList, "\n")),
			)
		}
	}

	return &api.VCSSQLReviewResult{
		Status: status,
		Content: []string{
			fmt.Sprintf(
				"<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<testsuites name=\"SQL Review\">\n%s\n</testsuites>",
				strings.Join(testsuiteList, "\n"),
			),
		},
	}
}

// convertSQLAdiceToGitHubActionResult will convert SQL advice map to GitHub action output format.
// GitHub action output message: https://docs.github.com/en/actions/using-workflows/workflow-commands-for-github-actions
// nolint:unused
func convertSQLAdiceToGitHubActionResult(adviceMap map[string][]advisor.Advice) *api.VCSSQLReviewResult {
	messageList := []string{}
	status := advisor.Success

	fileList := []string{}
	for filePath := range adviceMap {
		fileList = append(fileList, filePath)
	}
	sort.Strings(fileList)

	for _, filePath := range fileList {
		adviceList := adviceMap[filePath]
		for _, advice := range adviceList {
			if advice.Code == 0 || advice.Status == advisor.Success {
				continue
			}

			line := advice.Line
			if line <= 0 {
				line = 1
			}

			prefix := ""
			if advice.Status == advisor.Error {
				prefix = "error"
				status = advice.Status
			} else {
				prefix = "warning"
				if status != advisor.Error {
					status = advice.Status
				}
			}

			msg := fmt.Sprintf(
				"::%s file=%s,line=%d,col=1,endColumn=2,title=%s (%d)::%s\nDoc: %s#%d",
				prefix,
				filePath,
				line,
				advice.Title,
				advice.Code,
				advice.Content,
				sqlReviewDocs,
				advice.Code,
			)
			// To indent the output message in action
			messageList = append(messageList, strings.ReplaceAll(msg, "\n", "%0A"))
		}
	}
	return &api.VCSSQLReviewResult{
		Status:  status,
		Content: messageList,
	}
}
