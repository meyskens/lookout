package sdk_test

import (
	"github.com/meyskens/lookout/util/cmdtest"

	fixtures "github.com/meyskens/lookout-test-fixtures"
	git "gopkg.in/src-d/go-git.v4"
)

type SdkIntegrationSuite struct {
	cmdtest.IntegrationSuite

	gitPath string
}

func (s *SdkIntegrationSuite) SetupSuite() {
	s.gitPath = "/tmp/lookout-sdk-test"

	// clone repository with fixtures
	// we should update it after https://github.com/meyskens/lookout/issues/226 is resolved
	repo, err := git.PlainOpen(s.gitPath)
	if err == git.ErrRepositoryNotExists {
		_, err = git.PlainClone(s.gitPath, false, &git.CloneOptions{
			URL: "https://github.com/meyskens/lookout-test-fixtures.git",
		})

		if err != nil {
			s.FailNow("can't clone test repository", err.Error())
		}

		return
	}
	if err != nil {
		s.FailNow("can't open test repository", err.Error())
	}

	if err := repo.Fetch(&git.FetchOptions{}); err != nil && err != git.NoErrAlreadyUpToDate {
		s.FailNow("can't fetch test repository", err.Error())
	}
}

var longLineFixture = fixtures.GetByName("new-go-file-too-long-line")
var logLineRevision = longLineFixture.GetCommitRevision()
