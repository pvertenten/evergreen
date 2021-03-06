package command

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/evergreen-ci/evergreen/model"
	"github.com/evergreen-ci/evergreen/rest/client"
	"github.com/evergreen-ci/evergreen/util"
	"github.com/evergreen-ci/pail"
	"github.com/evergreen-ci/utility"
	"github.com/mitchellh/mapstructure"
	"github.com/mongodb/grip"
	"github.com/pkg/errors"
)

type s3Base struct {
	// BuildVariants contains all build variants this command should be run on.
	BuildVariants []string `mapstructure:"build_variants"`
	// ExcludeFilter contains a regexp describing files that should be
	// excluded from the operation.
	ExcludeFilter string `mapstructure:"exclude" plugin:"expand"`
	MaxRetries    uint   `mapstructure:"max_retries"`

	bucket pail.Bucket
}

func (c *s3Base) ParseParams(params map[string]interface{}) error {
	return errors.Wrapf(mapstructure.Decode(params, c), "error decoding S3 parameters")
}

func (c *s3Base) shouldRunOnBuildVariant(bv string) bool {
	if len(c.BuildVariants) == 0 {
		return true
	}

	return utility.StringSliceContains(c.BuildVariants, bv)
}

func (c *s3Base) expandParams(conf *model.TaskConfig) error {
	return errors.WithStack(util.ExpandValues(c, conf.Expansions))
}

func (c *s3Base) createBucket(client *http.Client, conf *model.TaskConfig, parallelOpts pail.ParallelBucketOptions) error {
	if c.bucket != nil {
		return nil
	}

	if err := conf.TaskSync.Validate(); err != nil {
		return errors.Wrap(err, "invalid credentials for task sync")
	}

	opts := pail.S3Options{
		Credentials: pail.CreateAWSCredentials(conf.TaskSync.Key, conf.TaskSync.Secret, ""),
		Region:      endpoints.UsEast1RegionID,
		Name:        conf.TaskSync.Bucket,
		MaxRetries:  int(c.MaxRetries),
		Permissions: pail.S3PermissionsPrivate,
	}
	bucket, err := pail.NewS3MultiPartBucketWithHTTPClient(client, opts)
	if err != nil {
		return errors.Wrap(err, "could not create bucket")
	}
	bucket = pail.NewParallelSyncBucket(parallelOpts, bucket)
	c.bucket = bucket

	return nil
}

// s3Pull is a command to download the task directory from S3.
type s3Pull struct {
	s3Base
	base

	FromBuildVariant string `mapstructure:"from_build_variant" plugin:"expand"`
	Task             string `mapstructure:"task" plugin:"expand"`
	WorkingDir       string `mapstructure:"working_directory" plugin:"expand"`
	DeleteOnSync     bool   `mapstructure:"delete_on_sync"`
}

func s3PullFactory() Command { return &s3Pull{} }

func (*s3Pull) Name() string {
	return "s3.pull"
}

func (c *s3Pull) ParseParams(params map[string]interface{}) error {
	if err := c.s3Base.ParseParams(params); err != nil {
		return errors.Wrapf(err, "error decoding %s params", c.Name())
	}
	if err := mapstructure.Decode(params, c); err != nil {
		return errors.Wrapf(err, "error decoding %s params", c.Name())
	}
	if c.Task == "" {
		return errors.New("task must not be empty")
	}
	if c.WorkingDir == "" {
		return errors.New("working directory cannot be empty")
	}
	return nil
}

func (c *s3Pull) expandParams(conf *model.TaskConfig) error {
	catcher := grip.NewBasicCatcher()
	catcher.Add(c.s3Base.expandParams(conf))
	catcher.Add(util.ExpandValues(c, conf.Expansions))
	return catcher.Resolve()
}

func (c *s3Pull) Execute(ctx context.Context, comm client.Communicator, logger client.LoggerProducer, conf *model.TaskConfig) error {
	if err := c.expandParams(conf); err != nil {
		return errors.Wrap(err, "error applying expansions to parameters")
	}

	if !c.shouldRunOnBuildVariant(conf.BuildVariant.Name) {
		logger.Task().Infof("Skipping s3.pull for build variant '%s'", conf.BuildVariant.Name)
		return nil
	}

	// If no buildvariant is explicitly stated, pull from this task's
	// build variant.
	if c.FromBuildVariant == "" {
		c.FromBuildVariant = conf.Task.BuildVariant
	}

	httpClient := utility.GetHTTPClient()
	defer utility.PutHTTPClient(httpClient)

	if err := c.createBucket(httpClient, conf, pail.ParallelBucketOptions{
		Workers:      runtime.NumCPU(),
		DeleteOnSync: c.DeleteOnSync,
	}); err != nil {
		return errors.Wrap(err, "could not set up S3 task bucket")
	}
	if err := c.bucket.Check(ctx); err != nil {
		return errors.Wrap(err, "could not find S3 task bucket")
	}

	logger.Execution().WarningWhen(filepath.IsAbs(c.WorkingDir) && !strings.HasPrefix(c.WorkingDir, conf.WorkDir),
		fmt.Sprintf("the working directory ('%s') is an absolute path, which isn't supported except when prefixed by '%s'",
			c.WorkingDir, conf.WorkDir))

	pullMsg := "Pulling task directory files from S3"
	if c.ExcludeFilter != "" {
		pullMsg += ", excluding files matching filter " + c.ExcludeFilter
	}
	logger.Task().Infof(pullMsg)
	if err := c.bucket.Pull(ctx, pail.SyncOptions{
		Local:   c.WorkingDir,
		Remote:  conf.Task.S3Path(c.FromBuildVariant, c.Task),
		Exclude: c.ExcludeFilter,
	}); err != nil {
		return errors.Wrap(err, "error pulling task data from S3")
	}

	return nil
}
