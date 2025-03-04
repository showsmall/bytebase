package tests

import (
	"context"
	"log"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	resourcemysql "github.com/bytebase/bytebase/resources/mysql"
	"github.com/bytebase/bytebase/resources/mysqlutil"

	"github.com/bytebase/bytebase/resources/postgres"
	"github.com/bytebase/bytebase/tests/fake"
)

func TestServiceRestart(t *testing.T) {
	t.Parallel()
	a := require.New(t)
	ctx := context.Background()
	ctl := &controller{}
	dataDir := t.TempDir()
	err := ctl.StartServer(ctx, &config{
		dataDir:            dataDir,
		vcsProviderCreator: fake.NewGitLab,
	})
	a.NoError(err)

	err = ctl.Login()
	a.NoError(err)

	projects, err := ctl.getProjects()
	a.NoError(err)

	// Test seed should have more than one project.
	a.Greater(len(projects), 1)

	// Restart the server.
	err = ctl.Close(ctx)
	a.NoError(err)

	err = ctl.StartServer(ctx, &config{
		dataDir:            dataDir,
		vcsProviderCreator: fake.NewGitLab,
	})
	a.NoError(err)
	defer ctl.Close(ctx)

	err = ctl.Login()
	a.NoError(err)
}

var (
	mysqlBinDir string
)

func TestMain(m *testing.M) {
	resourceDirOverride = os.TempDir()
	dir, err := postgres.Install(resourceDirOverride)
	if err != nil {
		log.Fatal(err)
	}
	externalPgBinDir = dir
	if _, err := mysqlutil.Install(resourceDirOverride); err != nil {
		log.Fatal(err)
	}
	dir, err = resourcemysql.Install(resourceDirOverride)
	if err != nil {
		log.Fatal(err)
	}
	mysqlBinDir = dir

	dir, err = os.MkdirTemp("", "bbtest-pgdata")
	if err != nil {
		log.Fatal(err)
	}
	externalPgDataDir = dir
	if err := postgres.InitDB(externalPgBinDir, externalPgDataDir, externalPgUser); err != nil {
		log.Fatal(err)
	}
	if err = postgres.Start(externalPgPort, externalPgBinDir, externalPgDataDir); err != nil {
		log.Fatal(err)
	}

	code := m.Run()

	// Graceful shutdown.
	if err := postgres.Stop(externalPgBinDir, externalPgDataDir); err != nil {
		log.Fatal(err)
	}
	if err := os.RemoveAll(externalPgDataDir); err != nil {
		log.Fatal(err)
	}

	os.Exit(code)
}
