package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonapi"
	"github.com/labstack/echo/v4"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/bytebase/bytebase/api"
	"github.com/bytebase/bytebase/common"
	"github.com/bytebase/bytebase/common/log"
	vcsPlugin "github.com/bytebase/bytebase/plugin/vcs"
	"github.com/bytebase/bytebase/plugin/vcs/github"
	"github.com/bytebase/bytebase/plugin/vcs/gitlab"
	"github.com/bytebase/bytebase/server/utils"
)

const (
	// sqlReviewInVCSPRTitle is the pull request title for SQL review CI setup.
	sqlReviewInVCSPRTitle = "chore: setup SQL review CI for Bytebase"
)

func (s *Server) registerProjectRoutes(g *echo.Group) {
	g.POST("/project", func(c echo.Context) error {
		ctx := c.Request().Context()
		projectCreate := &api.ProjectCreate{}
		if err := jsonapi.UnmarshalPayload(c.Request().Body, projectCreate); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Malformed create project request").SetInternal(err)
		}
		if projectCreate.Key == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "Project key cannot be empty")
		}
		if projectCreate.TenantMode == api.TenantModeTenant && !s.licenseService.IsFeatureEnabled(api.FeatureMultiTenancy) {
			return echo.NewHTTPError(http.StatusForbidden, api.FeatureMultiTenancy.AccessErrorMessage())
		}
		projectCreate.CreatorID = c.Get(getPrincipalIDContextKey()).(int)
		if projectCreate.TenantMode == "" {
			projectCreate.TenantMode = api.TenantModeDisabled
		}
		if err := api.ValidateProjectDBNameTemplate(projectCreate.DBNameTemplate); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Malformed create project request: %s", err.Error()))
		}
		if projectCreate.TenantMode != api.TenantModeTenant && projectCreate.DBNameTemplate != "" {
			return echo.NewHTTPError(http.StatusBadRequest, "database name template can only be set for tenant mode project")
		}
		project, err := s.store.CreateProject(ctx, projectCreate)
		if err != nil {
			if common.ErrorCode(err) == common.Conflict {
				return echo.NewHTTPError(http.StatusConflict, fmt.Sprintf("Project name already exists: %s", projectCreate.Name))
			}
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to create project").SetInternal(err)
		}

		projectMember := &api.ProjectMemberCreate{
			CreatorID:   projectCreate.CreatorID,
			ProjectID:   project.ID,
			Role:        common.ProjectOwner,
			PrincipalID: projectCreate.CreatorID,
		}

		if _, err = s.store.CreateProjectMember(ctx, projectMember); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to add owner after creating project").SetInternal(err)
		}

		c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
		if err := jsonapi.MarshalPayload(c.Response().Writer, project); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to marshal create project response").SetInternal(err)
		}
		return nil
	})

	g.GET("/project", func(c echo.Context) error {
		ctx := c.Request().Context()
		projectFind := &api.ProjectFind{}
		if userIDStr := c.QueryParam("user"); userIDStr != "" {
			userID, err := strconv.Atoi(userIDStr)
			if err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Query parameter user is not a number: %s", userIDStr)).SetInternal(err)
			}
			projectFind.PrincipalID = &userID
		}

		if rowStatusStr := c.QueryParam("rowstatus"); rowStatusStr != "" {
			rowStatus := api.RowStatus(rowStatusStr)
			projectFind.RowStatus = &rowStatus
		}
		projectList, err := s.store.FindProject(ctx, projectFind)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to fetch project list").SetInternal(err)
		}

		var activeProjectList []*api.Project
		// If principalID is passed, We will filter those projects with the current principal having the role provider
		// different from the project's current role provider.
		if projectFind.PrincipalID != nil {
			principalID := *projectFind.PrincipalID
			for _, project := range projectList {
				if api.HasActiveProjectMembership(principalID, project) {
					activeProjectList = append(activeProjectList, project)
				}
			}
		} else {
			activeProjectList = projectList
		}

		c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
		if err := jsonapi.MarshalPayload(c.Response().Writer, activeProjectList); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to marshal project list response").SetInternal(err)
		}
		return nil
	})

	g.GET("/project/:projectID", func(c echo.Context) error {
		ctx := c.Request().Context()
		id, err := strconv.Atoi(c.Param("projectID"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("ID is not a number: %s", c.Param("projectID"))).SetInternal(err)
		}

		project, err := s.store.GetProjectByID(ctx, id)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to fetch project ID: %v", id)).SetInternal(err)
		}
		if project == nil {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("Project not found with ID %d", id))
		}

		c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
		if err := jsonapi.MarshalPayload(c.Response().Writer, project); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to marshal project ID response: %v", id)).SetInternal(err)
		}
		return nil
	})

	g.PATCH("/project/:projectID", func(c echo.Context) error {
		ctx := c.Request().Context()
		id, err := strconv.Atoi(c.Param("projectID"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("ID is not a number: %s", c.Param("projectID"))).SetInternal(err)
		}

		projectPatch := &api.ProjectPatch{
			ID:        id,
			UpdaterID: c.Get(getPrincipalIDContextKey()).(int),
		}
		if err := jsonapi.UnmarshalPayload(c.Request().Body, projectPatch); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Malformed patch project request").SetInternal(err)
		}

		if v := projectPatch.Key; v != nil && *v == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "Project key cannot be empty")
		}
		if v := projectPatch.LGTMCheckSetting; v != nil {
			if !s.licenseService.IsFeatureEnabled(api.FeatureLGTM) {
				return echo.NewHTTPError(http.StatusBadRequest, api.FeatureLGTM.AccessErrorMessage())
			}
			if v.Value != api.LGTMValueDisabled && v.Value != api.LGTMValueProjectMember && v.Value != api.LGTMValueProjectOwner {
				return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Invalid LGTM check setting value: %v", v.Value))
			}
		}
		if v := projectPatch.TenantMode; v != nil {
			if *v == api.TenantModeTenant && !s.licenseService.IsFeatureEnabled(api.FeatureMultiTenancy) {
				return echo.NewHTTPError(http.StatusForbidden, api.FeatureMultiTenancy.AccessErrorMessage())
			}
		}
		if v := projectPatch.DBNameTemplate; v != nil {
			if err := api.ValidateProjectDBNameTemplate(*v); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Malformed patch project request: %s", err.Error()))
			}
		}

		// Verify before archiving the project:
		// 1. the project has no database.
		// 2. the issue status of this project should be canceled or done.
		if v := projectPatch.RowStatus; v != nil && *v == string(api.Archived) {
			databases, err := s.store.FindDatabase(ctx, &api.DatabaseFind{ProjectID: &id})
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, errors.Errorf("failed to find databases in the project %d", id)).SetInternal(err)
			}
			if len(databases) > 0 {
				return echo.NewHTTPError(http.StatusBadRequest, "Please transfer all databases under the project before archiving the project.")
			}

			issueList, err := s.store.FindIssueStripped(ctx, &api.IssueFind{ProjectID: &id, StatusList: []api.IssueStatus{api.IssueOpen}})
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, errors.Errorf("failed to find issues in the project %d", id)).SetInternal(err)
			}
			if len(issueList) > 0 {
				return echo.NewHTTPError(http.StatusBadRequest, "Please resolve all the issues in it before archiving the project.")
			}
		}

		project, err := s.store.PatchProject(ctx, projectPatch)
		if err != nil {
			if common.ErrorCode(err) == common.NotFound {
				return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("Project not found with ID %d", id))
			}
			if common.ErrorCode(err) == common.Conflict {
				return echo.NewHTTPError(http.StatusConflict, errors.Cause(err).Error())
			}
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to patch project with ID %v", id)).SetInternal(err)
		}

		c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
		if err := jsonapi.MarshalPayload(c.Response().Writer, project); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to marshal project ID response: %v", id)).SetInternal(err)
		}
		return nil
	})

	// When we link the repository with the project, we will also change the project workflow type to VCS
	g.POST("/project/:projectID/repository", func(c echo.Context) error {
		ctx := c.Request().Context()
		projectID, err := strconv.Atoi(c.Param("projectID"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("ID is not a number: %s", c.Param("projectID"))).SetInternal(err)
		}
		repositoryCreate := &api.RepositoryCreate{
			ProjectID: projectID,
			CreatorID: c.Get(getPrincipalIDContextKey()).(int),
		}
		if err := jsonapi.UnmarshalPayload(c.Request().Body, repositoryCreate); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Malformed create linked repository request").SetInternal(err)
		}
		if repositoryCreate.BranchFilter == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "Branch must be specified.")
		}

		hasWildcard := strings.Contains(repositoryCreate.BranchFilter, "*")
		if hasWildcard && repositoryCreate.SchemaPathTemplate != "" {
			return echo.NewHTTPError(http.StatusBadRequest, "Schema path template is supported only if branch doesn't have wildcard.")
		}

		// We need to check the FilePathTemplate in create repository request.
		// This avoids to a certain extent that the creation succeeds but does not work.
		if err := vcsPlugin.IsAsterisksInTemplateValid(path.Join(repositoryCreate.BaseDirectory, repositoryCreate.FilePathTemplate)); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, errors.Wrap(err, errors.Wrap(err, "Invalid base directory and filepath template combination").Error()))
		}

		project, err := s.store.GetProjectByID(ctx, projectID)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to fetch project ID: %v", projectID)).SetInternal(err)
		}
		if project == nil {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("Project not found with ID %d", projectID))
		}

		if err := api.ValidateRepositoryFilePathTemplate(repositoryCreate.FilePathTemplate, project.TenantMode, project.DBNameTemplate); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Malformed create linked repository request: %s", err.Error()))
		}

		if err := api.ValidateRepositorySchemaPathTemplate(repositoryCreate.SchemaPathTemplate, project.TenantMode); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Malformed create linked repository request: %s", err.Error()))
		}

		vcs, err := s.store.GetVCSByID(ctx, repositoryCreate.VCSID)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to find VCS for creating repository: %d", repositoryCreate.VCSID)).SetInternal(err)
		}
		if vcs == nil {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("VCS not found with ID: %d", repositoryCreate.VCSID))
		}

		// When the branch names doesn't contain wildcards, we should make sure the branch exists in the repo.
		if !hasWildcard {
			if notFound, err := isBranchNotFound(ctx, vcs, repositoryCreate.AccessToken, repositoryCreate.RefreshToken, repositoryCreate.ExternalID, repositoryCreate.BranchFilter); notFound {
				return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("Branch %q not found in repository %s.", repositoryCreate.BranchFilter, repositoryCreate.Name)).SetInternal(err)
			} else if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to get branch %q", repositoryCreate.BranchFilter)).SetInternal(err)
			}
		}

		// For a particular VCS repo, all Bytebase projects share the same webhook.
		repositories, err := s.store.FindRepository(ctx, &api.RepositoryFind{
			WebURL: &repositoryCreate.WebURL,
		})
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to find repository with web url: %s", repositoryCreate.WebURL)).SetInternal(err)
		}

		repositoryCreate.WebhookURLHost = s.profile.ExternalURL
		// If we can find at least one repository with the same web url, we will use the same webhook instead of creating a new one.
		if len(repositories) > 0 {
			repo := repositories[0]
			repositoryCreate.WebhookEndpointID = repo.WebhookEndpointID
			repositoryCreate.WebhookSecretToken = repo.WebhookSecretToken
			repositoryCreate.ExternalWebhookID = repo.ExternalWebhookID
		} else {
			repositoryCreate.WebhookEndpointID = fmt.Sprintf("%s-%d", s.workspaceID, time.Now().Unix())
			secretToken, err := common.RandomString(gitlab.SecretTokenLength)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, "Failed to generate random secret token for VCS").SetInternal(err)
			}
			repositoryCreate.WebhookSecretToken = secretToken

			webhookID, err := s.createVCSWebhook(ctx, vcs.Type, repositoryCreate.WebhookEndpointID, secretToken, repositoryCreate.AccessToken, vcs.InstanceURL, repositoryCreate.ExternalID)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to create webhook for project ID: %v", repositoryCreate.ProjectID)).SetInternal(err)
			}
			repositoryCreate.ExternalWebhookID = webhookID
		}
		// Remove enclosing /
		repositoryCreate.BaseDirectory = strings.Trim(repositoryCreate.BaseDirectory, "/")
		repository, err := s.store.CreateRepository(ctx, repositoryCreate)
		if err != nil {
			if common.ErrorCode(err) == common.Conflict {
				return echo.NewHTTPError(http.StatusConflict, fmt.Sprintf("Project %d has already linked repository", repositoryCreate.ProjectID))
			}
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to link project repository").SetInternal(err)
		}

		c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
		if err := jsonapi.MarshalPayload(c.Response().Writer, repository); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to marshal link project repository response").SetInternal(err)
		}
		return nil
	})

	g.POST("/project/:projectID/repository/:repositoryID/sql-review-ci", func(c echo.Context) error {
		ctx := c.Request().Context()
		projectID, err := strconv.Atoi(c.Param("projectID"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Project ID is not a number: %s", c.Param("projectID"))).SetInternal(err)
		}
		repositoryID, err := strconv.Atoi(c.Param("repositoryID"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Repository ID is not a number: %s", c.Param("repositoryID"))).SetInternal(err)
		}

		repository, err := s.store.GetRepository(ctx, &api.RepositoryFind{
			ID:        &repositoryID,
			ProjectID: &projectID,
		})
		if err != nil {
			if common.ErrorCode(err) == common.NotFound {
				return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("Cannot found repository %d in project %d", repositoryID, projectID)).SetInternal(err)
			}
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to find repository %d in project %d", repositoryID, projectID)).SetInternal(err)
		}

		if repository.Project.RowStatus == api.Archived {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Project %d is archived", projectID))
		}

		if !s.licenseService.IsFeatureEnabled(api.FeatureVCSSQLReviewWorkflow) {
			return echo.NewHTTPError(http.StatusForbidden, api.FeatureVCSSQLReviewWorkflow.AccessErrorMessage())
		}

		if repository.EnableSQLReviewCI {
			return echo.NewHTTPError(http.StatusBadRequest, "SQL review CI is already enabled")
		}

		pullRequest, err := s.setupVCSSQLReviewCI(ctx, repository)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to create SQL review CI").SetInternal(err)
		}

		response := &api.SQLReviewCISetup{
			PullRequestURL: pullRequest.URL,
		}

		enabledCI := true
		repoPatch := &api.RepositoryPatch{
			ID:                &repository.ID,
			UpdaterID:         c.Get(getPrincipalIDContextKey()).(int),
			EnableSQLReviewCI: &enabledCI,
		}
		if _, err := s.store.PatchRepository(ctx, repoPatch); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to update repository: %d", repository.ID)).SetInternal(err)
		}

		c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
		if err := jsonapi.MarshalPayload(c.Response().Writer, response); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to marshal response").SetInternal(err)
		}
		return nil
	})

	// Requires a separate API to return the repository, we do this because
	// 1. repository also contains project, which would cause circular dependency when composing it.
	// 2. repository info is only needed when fetching a particular project by id, thus it's unnecessary to include it in the project list response.
	g.GET("/project/:projectID/repository", func(c echo.Context) error {
		ctx := c.Request().Context()
		projectID, err := strconv.Atoi(c.Param("projectID"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Project ID is not a number: %s", c.Param("projectID"))).SetInternal(err)
		}

		repoFind := &api.RepositoryFind{
			ProjectID: &projectID,
		}
		repoList, err := s.store.FindRepository(ctx, repoFind)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to fetch repository list for project ID: %d", projectID)).SetInternal(err)
		}

		// Just be defensive, this shouldn't happen because we set UNIQUE constraint on project_id
		if len(repoList) > 1 {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Retrieved %d repository list for project ID: %d, expect at most 1", len(repoList), projectID)).SetInternal(err)
		}

		c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
		if err := jsonapi.MarshalPayload(c.Response().Writer, repoList); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to marshal project repository response: %v", projectID)).SetInternal(err)
		}
		return nil
	})

	// When we unlink the repository with the project, we will also change the project workflow type to UI
	g.PATCH("/project/:projectID/repository", func(c echo.Context) error {
		ctx := c.Request().Context()
		projectID, err := strconv.Atoi(c.Param("projectID"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Project ID is not a number: %s", c.Param("projectID"))).SetInternal(err)
		}
		repoPatch := &api.RepositoryPatch{
			UpdaterID: c.Get(getPrincipalIDContextKey()).(int),
		}
		if err := jsonapi.UnmarshalPayload(c.Request().Body, repoPatch); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Malformed patch linked repository request").SetInternal(err)
		}

		project, err := s.store.GetProjectByID(ctx, projectID)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to fetch project ID: %v", projectID)).SetInternal(err)
		}
		if project == nil {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("Project not found with ID %d", projectID))
		}

		if repoPatch.FilePathTemplate != nil {
			if err := api.ValidateRepositoryFilePathTemplate(*repoPatch.FilePathTemplate, project.TenantMode, project.DBNameTemplate); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Malformed patch linked repository request: %s", err.Error()))
			}
		}

		if repoPatch.SchemaPathTemplate != nil {
			if err := api.ValidateRepositorySchemaPathTemplate(*repoPatch.SchemaPathTemplate, project.TenantMode); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Malformed create linked repository request: %s", err.Error()))
			}
		}

		// Remove enclosing /
		if repoPatch.BaseDirectory != nil {
			baseDir := strings.Trim(*repoPatch.BaseDirectory, "/")
			repoPatch.BaseDirectory = &baseDir
		}

		repoFind := &api.RepositoryFind{
			ProjectID: &projectID,
		}
		repoList, err := s.store.FindRepository(ctx, repoFind)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to fetch repository list for project ID: %d", projectID)).SetInternal(err)
		}

		// Just be defensive, this shouldn't happen because we set UNIQUE constraint on project_id
		if len(repoList) > 1 {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Retrieved %d repository list for project ID: %d, expect at most 1", len(repoList), projectID)).SetInternal(err)
		} else if len(repoList) == 0 {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("Repository not found for project ID: %d", projectID))
		}

		repo := repoList[0]
		repoPatch.ID = &repo.ID
		newSchemaPathTemplate, newBranchFilter := repo.SchemaPathTemplate, repo.BranchFilter
		if repoPatch.SchemaPathTemplate != nil {
			newSchemaPathTemplate = *repoPatch.SchemaPathTemplate
		}
		if repoPatch.BranchFilter != nil {
			newBranchFilter = *repoPatch.BranchFilter
		}
		if newBranchFilter == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "Branch must be specified.")
		}

		hasWildcard := strings.Contains(newBranchFilter, "*")
		if hasWildcard && newSchemaPathTemplate != "" {
			return echo.NewHTTPError(http.StatusBadRequest, "Schema path template is supported only if branch doesn't have wildcard.")
		}

		vcs, err := s.store.GetVCSByID(ctx, repo.VCSID)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to find VCS for creating repository: %d", repo.VCSID)).SetInternal(err)
		}
		if vcs == nil {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("VCS not found with ID: %d", repo.VCSID))
		}

		// When the branch names doesn't contain wildcards, we should make sure the branch exists in the repo.
		if !hasWildcard {
			if notFound, err := isBranchNotFound(ctx, vcs, repo.AccessToken, repo.RefreshToken, repo.ExternalID, newBranchFilter); notFound {
				return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("Branch %q not found in repository %s.", newBranchFilter, repo.Name)).SetInternal(err)
			} else if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to get branch %q", newBranchFilter)).SetInternal(err)
			}
		}

		// We need to check the FilePathTemplate in create repository request.
		// This avoids to a certain extent that the creation succeeds but does not work.
		newBaseDirectory, newFilePathTemplate := repo.BaseDirectory, repo.FilePathTemplate
		if repoPatch.BaseDirectory != nil {
			newBaseDirectory = *repoPatch.BaseDirectory
		}
		if repoPatch.FilePathTemplate != nil {
			newFilePathTemplate = *repoPatch.FilePathTemplate
		}

		if err := vcsPlugin.IsAsterisksInTemplateValid(path.Join(newBaseDirectory, newFilePathTemplate)); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, errors.Wrap(err, "Invalid base directory and filepath template combination").Error())
		}

		// DO NOT enable the EnableSQLReviewCI field.
		// We will update it through POST /project/:projectID/repository/:repositoryID/sql-review-ci endpoint
		if repoPatch.EnableSQLReviewCI != nil && *repoPatch.EnableSQLReviewCI && !repo.EnableSQLReviewCI {
			repoPatch.EnableSQLReviewCI = &repo.EnableSQLReviewCI
		}

		updatedRepo, err := s.store.PatchRepository(ctx, repoPatch)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to update repository for project ID: %d", projectID)).SetInternal(err)
		}

		c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
		if err := jsonapi.MarshalPayload(c.Response().Writer, updatedRepo); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to marshal project repository response: %v", projectID)).SetInternal(err)
		}
		return nil
	})

	// When we unlink the repository with the project, we will also change the project workflow type to UI
	g.DELETE("/project/:projectID/repository", func(c echo.Context) error {
		ctx := c.Request().Context()
		projectID, err := strconv.Atoi(c.Param("projectID"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Project ID is not a number: %s", c.Param("projectID"))).SetInternal(err)
		}

		repositoryFind := &api.RepositoryFind{
			ProjectID: &projectID,
		}
		repoList, err := s.store.FindRepository(ctx, repositoryFind)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to fetch repository list for project ID: %d", projectID)).SetInternal(err)
		}

		// Just be defensive, this shouldn't happen because we set UNIQUE constraint on project_id
		if len(repoList) > 1 {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Retrieved %d repository list for project ID: %d, expect at most 1", len(repoList), projectID)).SetInternal(err)
		} else if len(repoList) == 0 {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("Repository not found for project ID: %d", projectID))
		}

		repo := repoList[0]
		vcs, err := s.store.GetVCSByID(ctx, repo.VCSID)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to delete repository for project ID: %d", projectID)).SetInternal(err)
		}
		if vcs == nil {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("VCS not found with ID: %d", repo.VCSID))
		}

		repositoryDelete := &api.RepositoryDelete{
			ProjectID: projectID,
			DeleterID: c.Get(getPrincipalIDContextKey()).(int),
		}
		if err := s.store.DeleteRepository(ctx, repositoryDelete); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to delete repository for project ID: %d", projectID)).SetInternal(err)
		}

		// We use one webhook in one repo for at least one Bytebase project, so we only delete the webhook if this project is the last one using this webhook.
		repos, err := s.store.FindRepository(ctx, &api.RepositoryFind{
			WebURL: &repo.WebURL,
		})
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to find repository for web url: %s", repo.WebURL)).SetInternal(err)
		}
		if len(repos) == 0 {
			// Delete the webhook after we successfully delete the repository.
			// This is because in case the webhook deletion fails, we can still have a cleanup process to cleanup the orphaned webhook.
			// If we delete it before we delete the repository, then if the repository deletion fails, we will have a broken repository with no webhook.
			if err = vcsPlugin.Get(vcs.Type, vcsPlugin.ProviderConfig{}).DeleteWebhook(
				ctx,
				// Need to get ApplicationID, Secret from vcs instead of repository.vcs since the latter is not composed.
				common.OauthContext{
					ClientID:     vcs.ApplicationID,
					ClientSecret: vcs.Secret,
					AccessToken:  repo.AccessToken,
					RefreshToken: repo.RefreshToken,
					Refresher:    refreshTokenNoop(),
				},
				vcs.InstanceURL,
				repo.ExternalID,
				repo.ExternalWebhookID,
			); err != nil {
				// Despite the error here, we have deleted the repository in the database, we still return success.
				log.Error("Failed to delete webhook for project", zap.Int("project", projectID), zap.Int("repo", repo.ID), zap.Error(err))
			}
		}

		c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
		c.Response().WriteHeader(http.StatusOK)
		return nil
	})

	g.PATCH("/project/:id/deployment", func(c echo.Context) error {
		ctx := c.Request().Context()
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("ID is not a number: %s", c.Param("id"))).SetInternal(err)
		}

		deploymentConfigUpsert := &api.DeploymentConfigUpsert{}
		if err := jsonapi.UnmarshalPayload(c.Request().Body, deploymentConfigUpsert); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Malformed set deployment configuration request").SetInternal(err)
		}
		deploymentConfigUpsert.UpdaterID = c.Get(getPrincipalIDContextKey()).(int)

		project, err := s.store.GetProjectByID(ctx, id)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to fetch project ID: %v", id)).SetInternal(err)
		}
		if project == nil {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("Project not found with ID %d", id))
		}
		deploymentConfigUpsert.ProjectID = id

		deploymentConfig, err := s.store.UpsertDeploymentConfig(ctx, deploymentConfigUpsert)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to set deployment configuration").SetInternal(err)
		}

		c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
		if err := jsonapi.MarshalPayload(c.Response().Writer, deploymentConfig); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to marshal set deployment configuration response").SetInternal(err)
		}
		return nil
	})

	g.GET("/project/:id/deployment", func(c echo.Context) error {
		ctx := c.Request().Context()
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("ID is not a number: %s", c.Param("id"))).SetInternal(err)
		}

		project, err := s.store.GetProjectByID(ctx, id)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to fetch project ID: %v", id)).SetInternal(err)
		}
		if project == nil {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("Project not found with ID %d", id))
		}

		// DeploymentConfig is never nil.
		deploymentConfig, err := s.store.GetDeploymentConfigByProjectID(ctx, id)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to get deployment configuration for project id: %d", id)).SetInternal(err)
		}

		c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
		if err := jsonapi.MarshalPayload(c.Response().Writer, &deploymentConfig); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to marshal get deployment configuration response: %v", id)).SetInternal(err)
		}
		return nil
	})

	g.POST("/project/:projectID/sync-sheet", func(c echo.Context) error {
		ctx := c.Request().Context()
		currentPrincipalID := c.Get(getPrincipalIDContextKey()).(int)
		projectID, err := strconv.Atoi(c.Param("projectID"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Project ID is not a number: %s", c.Param("projectID"))).SetInternal(err)
		}

		project, err := s.store.GetProjectByID(ctx, projectID)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Project not found: %d", projectID)).SetInternal(err)
		}
		if project == nil {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("Project not found by ID: %d", projectID))
		}
		if project.WorkflowType != api.VCSWorkflow {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Invalid workflow type: %s, need %s to enable this function", project.WorkflowType, api.VCSWorkflow))
		}

		repo, err := s.store.GetRepository(ctx, &api.RepositoryFind{ProjectID: &projectID})
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to find relevant VCS repo, Project ID: %d", projectID)).SetInternal(err)
		}
		if repo == nil {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("Repository not found by project ID: %d", projectID))
		}

		vcs, err := s.store.GetVCSByID(ctx, repo.VCSID)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to find VCS for sync sheet: %d", repo.VCSID)).SetInternal(err)
		}
		if vcs == nil {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("VCS not found by ID: %d", repo.VCSID))
		}

		basePath := filepath.Dir(repo.SheetPathTemplate)
		// TODO(Steven): The repo.branchFilter could be `test/*` which cannot be the ref value.
		fileList, err := vcsPlugin.Get(vcs.Type, vcsPlugin.ProviderConfig{}).FetchRepositoryFileList(ctx,
			common.OauthContext{
				ClientID:     vcs.ApplicationID,
				ClientSecret: vcs.Secret,
				AccessToken:  repo.AccessToken,
				RefreshToken: repo.RefreshToken,
				Refresher:    utils.RefreshToken(ctx, s.store, repo.WebURL),
			},
			vcs.InstanceURL,
			repo.ExternalID,
			repo.BranchFilter,
			basePath,
		)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to fetch repository file list from VCS, instance URL: %s", vcs.InstanceURL)).SetInternal(err)
		}

		for _, file := range fileList {
			sheetInfo, err := parseSheetInfo(file.Path, repo.SheetPathTemplate)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, "Failed to parse sheet info from template").SetInternal(err)
			}
			if sheetInfo.SheetName == "" {
				return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("sheet name cannot be empty from sheet path %s with template %s", file.Path, repo.SheetPathTemplate)).SetInternal(err)
			}

			fileContent, err := vcsPlugin.Get(vcs.Type, vcsPlugin.ProviderConfig{}).ReadFileContent(ctx,
				common.OauthContext{
					ClientID:     vcs.ApplicationID,
					ClientSecret: vcs.Secret,
					AccessToken:  repo.AccessToken,
					RefreshToken: repo.RefreshToken,
					Refresher:    utils.RefreshToken(ctx, s.store, repo.WebURL),
				},
				vcs.InstanceURL,
				repo.ExternalID,
				file.Path,
				repo.BranchFilter,
			)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to fetch file content from VCS, instance URL: %s, repo ID: %s, file path: %s, branch: %s", vcs.InstanceURL, repo.ExternalID, file.Path, repo.BranchFilter)).SetInternal(err)
			}

			fileMeta, err := vcsPlugin.Get(vcs.Type, vcsPlugin.ProviderConfig{}).ReadFileMeta(ctx,
				common.OauthContext{
					ClientID:     vcs.ApplicationID,
					ClientSecret: vcs.Secret,
					AccessToken:  repo.AccessToken,
					RefreshToken: repo.RefreshToken,
					Refresher:    utils.RefreshToken(ctx, s.store, repo.WebURL),
				},
				vcs.InstanceURL,
				repo.ExternalID,
				file.Path,
				repo.BranchFilter,
			)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to fetch file meta from VCS, instance URL: %s, repo ID: %s, file path: %s, branch: %s", vcs.InstanceURL, repo.ExternalID, file.Path, repo.BranchFilter)).SetInternal(err)
			}

			lastCommit, err := vcsPlugin.Get(vcs.Type, vcsPlugin.ProviderConfig{}).FetchCommitByID(ctx,
				common.OauthContext{
					ClientID:     vcs.ApplicationID,
					ClientSecret: vcs.Secret,
					AccessToken:  repo.AccessToken,
					RefreshToken: repo.RefreshToken,
					Refresher:    utils.RefreshToken(ctx, s.store, repo.WebURL),
				},
				vcs.InstanceURL,
				repo.ExternalID,
				fileMeta.LastCommitID,
			)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to fetch commit data from VCS, instance URL: %s, repo ID: %s, commit ID: %s", vcs.InstanceURL, repo.ExternalID, fileMeta.LastCommitID)).SetInternal(err)
			}

			sheetVCSPayload := &api.SheetVCSPayload{
				FileName:     fileMeta.Name,
				FilePath:     fileMeta.Path,
				Size:         fileMeta.Size,
				Author:       lastCommit.AuthorName,
				LastCommitID: lastCommit.ID,
				LastSyncTs:   time.Now().Unix(),
			}
			payload, err := json.Marshal(sheetVCSPayload)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, "Failed to marshal sheetVCSPayload").SetInternal(err)
			}

			var databaseID *int
			// In non-tenant mode, we can set a databaseId for sheet with ENV_NAME and DB_NAME,
			// and ENV_NAME and DB_NAME is either both present or neither present.
			if project.TenantMode != api.TenantModeDisabled {
				if sheetInfo.EnvironmentName != "" && sheetInfo.DatabaseName != "" {
					databaseList, err := s.store.FindDatabase(ctx, &api.DatabaseFind{
						Name:      &sheetInfo.DatabaseName,
						ProjectID: &projectID,
					})
					if err != nil {
						return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to find database list with name: %s, project ID: %d", sheetInfo.DatabaseName, projectID)).SetInternal(err)
					}

					for _, database := range databaseList {
						database := database // create a new var "database".
						if database.Instance.Environment.Name == sheetInfo.EnvironmentName {
							databaseID = &database.ID
							break
						}
					}
				}
			}

			var sheetSource api.SheetSource
			switch vcs.Type {
			case vcsPlugin.GitLabSelfHost:
				sheetSource = api.SheetFromGitLabSelfHost
			case vcsPlugin.GitHubCom:
				sheetSource = api.SheetFromGitHubCom
			}
			vscSheetType := api.SheetForSQL
			sheetFind := &api.SheetFind{
				Name:      &sheetInfo.SheetName,
				ProjectID: &project.ID,
				Source:    &sheetSource,
				Type:      &vscSheetType,
			}
			sheet, err := s.store.GetSheet(ctx, sheetFind, currentPrincipalID)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to find sheet with name: %s, project ID: %d", sheetInfo.SheetName, projectID)).SetInternal(err)
			}

			if sheet == nil {
				sheetCreate := api.SheetCreate{
					ProjectID:  projectID,
					CreatorID:  currentPrincipalID,
					Name:       sheetInfo.SheetName,
					Statement:  fileContent,
					Visibility: api.ProjectSheet,
					Source:     sheetSource,
					Type:       api.SheetForSQL,
					Payload:    string(payload),
				}
				if databaseID != nil {
					sheetCreate.DatabaseID = databaseID
				}

				if _, err := s.store.CreateSheet(ctx, &sheetCreate); err != nil {
					return echo.NewHTTPError(http.StatusInternalServerError, "Failed to create sheet from VCS").SetInternal(err)
				}
			} else {
				payloadString := string(payload)
				sheetPatch := api.SheetPatch{
					ID:        sheet.ID,
					UpdaterID: currentPrincipalID,
					Statement: &fileContent,
					Payload:   &payloadString,
				}
				if databaseID != nil {
					sheetPatch.DatabaseID = databaseID
				}

				if _, err := s.store.PatchSheet(ctx, &sheetPatch); err != nil {
					return echo.NewHTTPError(http.StatusInternalServerError, "Failed to patch sheet from VCS").SetInternal(err)
				}
			}
		}

		c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
		return nil
	})
}

func (s *Server) setupVCSSQLReviewCI(ctx context.Context, repository *api.Repository) (*vcsPlugin.PullRequest, error) {
	branch, err := s.setupVCSSQLReviewBranch(ctx, repository)
	if err != nil {
		return nil, err
	}

	if err := vcsPlugin.Get(repository.VCS.Type, vcsPlugin.ProviderConfig{}).UpsertEnvironmentVariable(
		ctx,
		common.OauthContext{
			ClientID:     repository.VCS.ApplicationID,
			ClientSecret: repository.VCS.Secret,
			AccessToken:  repository.AccessToken,
			RefreshToken: repository.RefreshToken,
			Refresher:    utils.RefreshToken(ctx, s.store, repository.WebURL),
		},
		repository.VCS.InstanceURL,
		repository.ExternalID,
		vcsPlugin.SQLReviewAPISecretName,
		repository.WebhookSecretToken,
	); err != nil {
		return nil, err
	}

	sqlReviewEndpoint := fmt.Sprintf("%s/hook/sql-review/%s", s.profile.ExternalURL, repository.WebhookEndpointID)

	switch repository.VCS.Type {
	case vcsPlugin.GitHubCom:
		if err := s.setupVCSSQLReviewCIForGitHub(ctx, repository, branch, sqlReviewEndpoint); err != nil {
			return nil, err
		}
	case vcsPlugin.GitLabSelfHost:
		if err := s.setupVCSSQLReviewCIForGitLab(ctx, repository, branch, sqlReviewEndpoint); err != nil {
			return nil, err
		}
	}

	return vcsPlugin.Get(repository.VCS.Type, vcsPlugin.ProviderConfig{}).CreatePullRequest(
		ctx,
		common.OauthContext{
			ClientID:     repository.VCS.ApplicationID,
			ClientSecret: repository.VCS.Secret,
			AccessToken:  repository.AccessToken,
			RefreshToken: repository.RefreshToken,
			Refresher:    utils.RefreshToken(ctx, s.store, repository.WebURL),
		},
		repository.VCS.InstanceURL,
		repository.ExternalID,
		&vcsPlugin.PullRequestCreate{
			Title:                 sqlReviewInVCSPRTitle,
			Body:                  "This pull request is auto-generated by Bytebase for GitOps workflow.",
			Head:                  branch.Name,
			Base:                  repository.BranchFilter,
			RemoveHeadAfterMerged: true,
		},
	)
}

// setupVCSSQLReviewBranch will create a new branch to setup SQL review CI.
func (s *Server) setupVCSSQLReviewBranch(ctx context.Context, repository *api.Repository) (*vcsPlugin.BranchInfo, error) {
	branch, err := vcsPlugin.Get(repository.VCS.Type, vcsPlugin.ProviderConfig{}).GetBranch(
		ctx,
		common.OauthContext{
			ClientID:     repository.VCS.ApplicationID,
			ClientSecret: repository.VCS.Secret,
			AccessToken:  repository.AccessToken,
			RefreshToken: repository.RefreshToken,
			Refresher:    utils.RefreshToken(ctx, s.store, repository.WebURL),
		},
		repository.VCS.InstanceURL,
		repository.ExternalID,
		repository.BranchFilter,
	)
	if err != nil {
		return nil, err
	}
	log.Debug("VCS target branch info", zap.String("last_commit", branch.LastCommitID), zap.String("name", branch.Name))

	branchCreate := &vcsPlugin.BranchInfo{
		Name:         fmt.Sprintf("bytebase-vcs-%d", time.Now().Unix()),
		LastCommitID: branch.LastCommitID,
	}
	if err := vcsPlugin.Get(repository.VCS.Type, vcsPlugin.ProviderConfig{}).CreateBranch(
		ctx,
		common.OauthContext{
			ClientID:     repository.VCS.ApplicationID,
			ClientSecret: repository.VCS.Secret,
			AccessToken:  repository.AccessToken,
			RefreshToken: repository.RefreshToken,
			Refresher:    utils.RefreshToken(ctx, s.store, repository.WebURL),
		},
		repository.VCS.InstanceURL,
		repository.ExternalID,
		branchCreate,
	); err != nil {
		return nil, err
	}

	return branchCreate, nil
}

// setupVCSSQLReviewCIForGitHub will create the pull request in GitHub to setup SQL review action.
func (s *Server) setupVCSSQLReviewCIForGitHub(ctx context.Context, repository *api.Repository, branch *vcsPlugin.BranchInfo, sqlReviewEndpoint string) error {
	sqlReviewConfig := github.SetupSQLReviewCI(sqlReviewEndpoint)
	fileLastCommitID := ""

	fileMeta, err := vcsPlugin.Get(repository.VCS.Type, vcsPlugin.ProviderConfig{}).ReadFileMeta(
		ctx,
		common.OauthContext{
			ClientID:     repository.VCS.ApplicationID,
			ClientSecret: repository.VCS.Secret,
			AccessToken:  repository.AccessToken,
			RefreshToken: repository.RefreshToken,
			Refresher:    utils.RefreshToken(ctx, s.store, repository.WebURL),
		},
		repository.VCS.InstanceURL,
		repository.ExternalID,
		github.SQLReviewActionFilePath,
		branch.Name,
	)
	if err != nil {
		log.Debug(
			"Failed to get file meta",
			zap.String("file", github.SQLReviewActionFilePath),
			zap.String("last_commit", branch.LastCommitID),
			zap.Int("code", common.ErrorCode(err).Int()),
			zap.Error(err),
		)
	} else if fileMeta != nil {
		fileLastCommitID = fileMeta.LastCommitID
	}

	return vcsPlugin.Get(repository.VCS.Type, vcsPlugin.ProviderConfig{}).CreateFile(
		ctx,
		common.OauthContext{
			ClientID:     repository.VCS.ApplicationID,
			ClientSecret: repository.VCS.Secret,
			AccessToken:  repository.AccessToken,
			RefreshToken: repository.RefreshToken,
			Refresher:    utils.RefreshToken(ctx, s.store, repository.WebURL),
		},
		repository.VCS.InstanceURL,
		repository.ExternalID,
		github.SQLReviewActionFilePath,
		vcsPlugin.FileCommitCreate{
			Branch:        branch.Name,
			CommitMessage: sqlReviewInVCSPRTitle,
			Content:       sqlReviewConfig,
			LastCommitID:  fileLastCommitID,
		},
	)
}

// setupVCSSQLReviewCIForGitLab will create or update SQL review related files in GitLab to setup SQL review CI.
func (s *Server) setupVCSSQLReviewCIForGitLab(ctx context.Context, repository *api.Repository, branch *vcsPlugin.BranchInfo, sqlReviewEndpoint string) error {
	// create or update the .gitlab-ci.yml
	if err := s.createOrUpdateVCSSQLReviewFileForGitLab(ctx, repository, branch, gitlab.CIFilePath, func(fileMeta *vcsPlugin.FileMeta) (string, error) {
		content := make(map[string]interface{})

		if fileMeta != nil {
			ciFileContent, err := vcsPlugin.Get(repository.VCS.Type, vcsPlugin.ProviderConfig{}).ReadFileContent(
				ctx,
				common.OauthContext{
					ClientID:     repository.VCS.ApplicationID,
					ClientSecret: repository.VCS.Secret,
					AccessToken:  repository.AccessToken,
					RefreshToken: repository.RefreshToken,
					Refresher:    utils.RefreshToken(ctx, s.store, repository.WebURL),
				},
				repository.VCS.InstanceURL,
				repository.ExternalID,
				gitlab.CIFilePath,
				fileMeta.LastCommitID,
			)
			if err != nil {
				return "", err
			}
			if err := yaml.Unmarshal([]byte(ciFileContent), &content); err != nil {
				return "", err
			}
		}

		newContent, err := gitlab.SetupGitLabCI(content)
		if err != nil {
			return "", err
		}

		return newContent, nil
	}); err != nil {
		return err
	}

	// create or update the SQL review CI.
	return s.createOrUpdateVCSSQLReviewFileForGitLab(ctx, repository, branch, gitlab.SQLReviewCIFilePath, func(_ *vcsPlugin.FileMeta) (string, error) {
		return gitlab.SetupSQLReviewCI(sqlReviewEndpoint), nil
	})
}

// createOrUpdateVCSSQLReviewFileForGitLab will create or update SQL review file for GitLab CI.
func (s *Server) createOrUpdateVCSSQLReviewFileForGitLab(
	ctx context.Context,
	repository *api.Repository,
	branch *vcsPlugin.BranchInfo,
	fileName string,
	getNewContent func(meta *vcsPlugin.FileMeta) (string, error),
) error {
	fileExisted := true
	fileMeta, err := vcsPlugin.Get(repository.VCS.Type, vcsPlugin.ProviderConfig{}).ReadFileMeta(
		ctx,
		common.OauthContext{
			ClientID:     repository.VCS.ApplicationID,
			ClientSecret: repository.VCS.Secret,
			AccessToken:  repository.AccessToken,
			RefreshToken: repository.RefreshToken,
			Refresher:    utils.RefreshToken(ctx, s.store, repository.WebURL),
		},
		repository.VCS.InstanceURL,
		repository.ExternalID,
		fileName,
		branch.Name,
	)
	if err != nil {
		log.Debug(
			"Failed to get file meta",
			zap.String("last_commit", branch.LastCommitID),
			zap.Int("code", common.ErrorCode(err).Int()),
			zap.Error(err),
		)
		if common.ErrorCode(err) == common.NotFound {
			fileExisted = false
		} else {
			return err
		}
	}

	newContent, err := getNewContent(fileMeta)
	if err != nil {
		return err
	}

	if fileExisted {
		return vcsPlugin.Get(repository.VCS.Type, vcsPlugin.ProviderConfig{}).OverwriteFile(
			ctx,
			common.OauthContext{
				ClientID:     repository.VCS.ApplicationID,
				ClientSecret: repository.VCS.Secret,
				AccessToken:  repository.AccessToken,
				RefreshToken: repository.RefreshToken,
				Refresher:    utils.RefreshToken(ctx, s.store, repository.WebURL),
			},
			repository.VCS.InstanceURL,
			repository.ExternalID,
			fileName,
			vcsPlugin.FileCommitCreate{
				Branch:        branch.Name,
				CommitMessage: sqlReviewInVCSPRTitle,
				Content:       newContent,
				LastCommitID:  fileMeta.LastCommitID,
			},
		)
	}

	return vcsPlugin.Get(repository.VCS.Type, vcsPlugin.ProviderConfig{}).CreateFile(
		ctx,
		common.OauthContext{
			ClientID:     repository.VCS.ApplicationID,
			ClientSecret: repository.VCS.Secret,
			AccessToken:  repository.AccessToken,
			RefreshToken: repository.RefreshToken,
			Refresher:    utils.RefreshToken(ctx, s.store, repository.WebURL),
		},
		repository.VCS.InstanceURL,
		repository.ExternalID,
		fileName,
		vcsPlugin.FileCommitCreate{
			Branch:        branch.Name,
			CommitMessage: sqlReviewInVCSPRTitle,
			Content:       newContent,
		},
	)
}

func (s *Server) createVCSWebhook(ctx context.Context, vcsType vcsPlugin.Type, webhookEndpointID, secretToken, accessToken, instanceURL, externalRepoID string) (string, error) {
	// Create a new webhook and retrieve the created webhook ID
	var webhookCreatePayload []byte
	var err error
	switch vcsType {
	case vcsPlugin.GitLabSelfHost:
		webhookCreate := gitlab.WebhookCreate{
			URL:                   fmt.Sprintf("%s/hook/gitlab/%s", s.profile.ExternalURL, webhookEndpointID),
			SecretToken:           secretToken,
			PushEvents:            true,
			EnableSSLVerification: false, // TODO(tianzhou): This is set to false, be lax to not enable_ssl_verification
		}
		webhookCreatePayload, err = json.Marshal(webhookCreate)
		if err != nil {
			return "", errors.Wrap(err, "failed to marshal request body for creating webhook")
		}
	case vcsPlugin.GitHubCom:
		webhookPost := github.WebhookCreateOrUpdate{
			Config: github.WebhookConfig{
				URL:         fmt.Sprintf("%s/hook/github/%s", s.profile.ExternalURL, webhookEndpointID),
				ContentType: "json",
				Secret:      secretToken,
				InsecureSSL: 1, // TODO: Allow user to specify this value through api.RepositoryCreate
			},
			Events: []string{"push"},
		}
		webhookCreatePayload, err = json.Marshal(webhookPost)
		if err != nil {
			return "", errors.Wrap(err, "failed to marshal request body for creating webhook")
		}
	}
	webhookID, err := vcsPlugin.Get(vcsType, vcsPlugin.ProviderConfig{}).CreateWebhook(
		ctx,
		common.OauthContext{
			AccessToken: accessToken,
			// We use refreshTokenNoop() because the repository isn't created yet.
			Refresher: refreshTokenNoop(),
		},
		instanceURL,
		externalRepoID,
		webhookCreatePayload,
	)
	if err != nil {
		return "", errors.Wrap(err, "failed to create webhook")
	}
	return webhookID, nil
}

// refreshToken is a no-op token refresher. It should be used when the repository isn't created yet.
func refreshTokenNoop() common.TokenRefresher {
	return func(newToken, newRefreshToken string, expiresTs int64) error {
		return nil
	}
}

func isBranchNotFound(ctx context.Context, vcs *api.VCS, accessToken, refreshToken, externalID, branch string) (bool, error) {
	_, err := vcsPlugin.Get(vcs.Type, vcsPlugin.ProviderConfig{}).GetBranch(ctx,
		common.OauthContext{
			ClientID:     vcs.ApplicationID,
			ClientSecret: vcs.Secret,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			Refresher:    nil,
		},
		vcs.InstanceURL, externalID, branch)

	if common.ErrorCode(err) == common.NotFound {
		return true, nil
	}
	return false, err
}
