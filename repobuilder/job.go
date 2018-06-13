package repobuilder

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goamz/goamz/s3"
	"github.com/mongodb/amboy"
	"github.com/mongodb/amboy/dependency"
	"github.com/mongodb/amboy/job"
	"github.com/mongodb/amboy/registry"
	"github.com/mongodb/curator"
	"github.com/mongodb/curator/sthree"
	"github.com/mongodb/grip"
	"github.com/pkg/errors"
)

type jobImpl interface {
	rebuildRepo(string) error
	injectPackage(string, string) (string, error)
}

// Job provides the common structure for a repository building Job.
type Job struct {
	Distro       *RepositoryDefinition `bson:"distro" json:"distro" yaml:"distro"`
	Conf         *RepositoryConfig     `bson:"conf" json:"conf" yaml:"conf"`
	DryRun       bool                  `bson:"dry_run" json:"dry_run" yaml:"dry_run"`
	Output       map[string]string     `bson:"output" json:"output" yaml:"output"`
	Version      string                `bson:"version" json:"version" yaml:"version"`
	Arch         string                `bson:"arch" json:"arch" yaml:"arch"`
	Profile      string                `bson:"aws_profile" json:"aws_profile" yaml:"aws_profile"`
	WorkSpace    string                `bson:"local_workdir" json:"local_workdir" yaml:"local_workdir"`
	PackagePaths []string              `bson:"package_paths" json:"package_paths" yaml:"package_paths"`
	*job.Base    `bson:"metadata" json:"metadata" yaml:"metadata"`

	workingDirs []string
	release     *curator.MongoDBVersion
	mutex       sync.RWMutex
	builder     jobImpl
}

func init() {
	registry.AddJobType("build-repo", func() amboy.Job {
		return buildRepoJob()
	})
}

func buildRepoJob() *Job {
	j := &Job{
		Output: make(map[string]string),
		Base: &job.Base{
			JobType: amboy.JobType{
				Name:    "build-repo",
				Version: 2,
			},
		},
	}

	j.SetDependency(dependency.NewAlways())

	return j
}

// NewBuildRepoJob constructs a repository building job, which
// implements the amboy.Job interface.
func NewBuildRepoJob(conf *RepositoryConfig, distro *RepositoryDefinition, version, arch, profile string, pkgs ...string) (*Job, error) {
	var err error

	j := buildRepoJob()
	if distro.Type == DEB {
		setupDEBJob(j)
	} else if distro.Type == RPM {
		setupRPMJob(j)
	}

	j.release, err = curator.NewMongoDBVersion(version)
	if err != nil {
		return nil, err
	}

	j.WorkSpace, err = os.Getwd()
	if err != nil {
		grip.Errorln("system error: cannot determine the current working directory.",
			"not creating a job object.")
		return nil, err
	}

	j.SetID(fmt.Sprintf("build-%s-repo.%d", distro.Type, job.GetNumber()))
	j.Arch = distro.getArchForDistro(arch)
	j.Distro = distro
	j.Conf = conf
	j.PackagePaths = pkgs
	j.Version = version
	j.Profile = profile

	return j, nil
}

func (j *Job) linkPackages(dest string) error {
	catcher := grip.NewCatcher()
	wg := &sync.WaitGroup{}
	defer wg.Wait()
	for _, pkg := range j.PackagePaths {
		if j.Distro.Type == DEB && !strings.HasSuffix(pkg, ".deb") {
			// the Packages files generated by the compile
			// task are caught in this glob. It's
			// harmless, as we regenerate these files
			// later, but just to be careful and more
			// clear, we should skip these files.
			continue
		}

		if _, err := os.Stat(dest); os.IsNotExist(err) {
			grip.Noticeln("creating directory:", dest)
			if err := os.MkdirAll(dest, 0744); err != nil {
				catcher.Add(errors.Wrapf(err, "problem creating directory %s",
					dest))
				continue
			}
		}

		mirror := filepath.Join(dest, filepath.Base(pkg))

		if _, err := os.Stat(mirror); os.IsNotExist(err) {
			grip.Infof("copying package %s to local staging %s", pkg, dest)

			if err = os.Link(pkg, mirror); err != nil {
				catcher.Add(errors.Wrapf(err, "problem copying package %s to %s",
					pkg, mirror))
				continue
			}

			if j.Distro.Type == RPM {
				wg.Add(1)
				go func(toSign string) {
					// sign each package, overwriting the package with the signed package.
					catcher.Add(errors.Wrapf(j.signFile(toSign, "", true), // (name, extension, overwrite)
						"problem signing file %s", toSign))
					wg.Done()
				}(mirror)
			}

		} else {
			grip.Infof("file %s is already mirrored", mirror)
		}
	}

	return catcher.Resolve()
}

func (j *Job) injectNewPackages(local string) (string, error) {
	return j.builder.injectPackage(local, j.getPackageLocation())
}

func (j *Job) getPackageLocation() string {
	if j.release.IsDevelopmentBuild() {
		// nightlies to the a "development" repo.
		return "development"
	} else if j.release.IsReleaseCandidate() {
		// release candidates go into the testing repo:
		return "testing"
	} else {
		// there are repos for each series:
		return j.release.Series()
	}
}

// signFile wraps the python notary-client.py script. Pass it the name
// of a file to sign, the "archiveExtension" (which only impacts
// non-package files, as defined by the notary service and client,)
// and an "overwrite" bool. Overwrite: forces package signing to
// overwrite the existing file, removing the archive's
// signature. Using overwrite=true and a non-nil string is not logical
// and returns a warning, but is passed to the client.
func (j *Job) signFile(fileName, archiveExtension string, overwrite bool) error {
	// In the future it would be nice if we could talk to the
	// notary service directly rather than shelling out here. The
	// final option controls if we overwrite this file.

	var keyName string
	var token string

	if j.Distro.Type == DEB && (j.release.Series() == "3.0" || j.release.Series() == "2.6") {
		keyName = "richard"
		token = os.Getenv("NOTARY_TOKEN_DEB_LEGACY")
	} else {
		keyName = "server-" + j.release.StableReleaseSeries()
		token = os.Getenv("NOTARY_TOKEN")
	}

	if token == "" {
		return errors.New(fmt.Sprintln("the notary service auth token",
			"(NOTARY_TOKEN) is not defined in the environment"))
	}

	args := []string{
		"notary-client.py",
		"--key-name", keyName,
		"--auth-token", token,
		"--comment", "\"curator package signing\"",
		"--notary-url", j.Conf.Services.NotaryURL,
		"--archive-file-ext", archiveExtension,
		"--outputs", "sig",
	}

	grip.AlertWhenf(strings.HasPrefix(archiveExtension, "."),
		"extension '%s', has a leading dot, which is almost certainly undesirable.", archiveExtension)

	grip.AlertWhenln(overwrite && len(archiveExtension) != 0,
		"specified overwrite with an archive extension:", archiveExtension,
		"this is probably an error, (not impacting packages,) but is passed to the client.")

	if overwrite {
		grip.Noticef("overwriting existing contents of file '%s' while signing it", fileName)
		args = append(args, "--package-file-suffix", "")
	} else {
		// if we're not overwriting the unsigned source file
		// with the signed file, then we should remove the
		// signed artifact before. Unclear if this is needed,
		// the cronjob did this.
		grip.CatchWarning(os.Remove(fileName + "." + archiveExtension))
	}

	args = append(args, filepath.Base(fileName))
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = filepath.Dir(fileName)

	grip.Infoln("running notary command:", strings.Replace(
		strings.Join(cmd.Args, " "),
		token, "XXXXX", -1))

	out, err := cmd.CombinedOutput()
	output := strings.Trim(string(out), " \n\t")

	if err != nil {
		grip.Warningf("error signed file '%s': %s (%s)",
			fileName, err.Error(), output)
		return errors.Wrap(err, "problem with notary service client signing file")
	}

	grip.Noticef("successfully signed file: %s (%s)", fileName, output)

	return nil
}

// Run is the main execution entry point into repository building, and is a component
func (j *Job) Run(ctx context.Context) {
	bucket := sthree.GetBucketWithProfile(j.Distro.Bucket, j.Profile)
	err := bucket.Open(ctx)
	if err != nil {
		j.AddError(errors.Wrapf(err, "opening bucket %s", bucket))
		return
	}
	defer bucket.Close()

	if j.DryRun {
		// the error (second argument) will be caught (when we
		// run open below)
		bucket, err = bucket.DryRunClone(ctx)
		if err != nil {
			j.AddError(errors.Wrapf(err,
				"problem getting bucket '%s' in dry-mode", bucket))
			return
		}

		err := bucket.Open(ctx)
		if err != nil {
			j.AddError(errors.Wrapf(err, "opening bucket %s [dry-run]", bucket))
			return
		}
		defer bucket.Close()
	}

	bucket.NewFilePermission = s3.PublicRead

	defer j.MarkComplete()
	wg := &sync.WaitGroup{}

	syncOpts := sthree.NewDefaultSyncOptions()
	if deadline, ok := ctx.Deadline(); ok {
		syncOpts.Timeout = time.Until(deadline)
	}
	// at the moment there is only multiple repos for RPM distros
	for _, remote := range j.Distro.Repos {
		clonedBucket, err := bucket.Clone(ctx)
		if err != nil {
			j.AddError(errors.Wrapf(err, "problem cloning bucket %s", bucket))
			continue
		}

		j.workingDirs = append(j.workingDirs, remote)

		wg.Add(1)
		go func(remote string, b *sthree.Bucket) {
			defer b.Close()
			grip.Infof("rebuilding %s.%s", b, remote)
			defer wg.Done()

			local := filepath.Join(j.WorkSpace, remote)

			var err error

			if err = os.MkdirAll(local, 0755); err != nil {
				j.AddError(errors.Wrapf(err, "creating directory %s", local))
				return
			}

			grip.Infof("downloading from %s to %s", remote, local)
			pkgLocation := j.getPackageLocation()
			if err = b.SyncFrom(ctx, filepath.Join(local, pkgLocation), filepath.Join(remote, pkgLocation), syncOpts); err != nil {
				j.AddError(errors.Wrapf(err, "sync from %s to %s", remote, local))
				return
			}

			grip.Info("copying new packages into local staging area")
			changed, err := j.injectNewPackages(local)
			if err != nil {
				j.AddError(errors.Wrap(err, "copying packages into staging repos"))
				return
			}

			// rebuildRepo may hold the lock (and does for
			// the bulk of the operation with RPM
			// distros.)
			if err = j.builder.rebuildRepo(changed); err != nil {
				j.AddError(errors.Wrapf(err, "problem building repo in '%s'", changed))
				return
			}

			var syncSource string
			var changedComponent string

			if j.Distro.Type == DEB {
				changedComponent = filepath.Dir(changed[len(local)+1:])
				syncSource = filepath.Dir(changed)
			} else if j.Distro.Type == RPM {
				changedComponent = changed[len(local)+1:]
				syncSource = changed
			} else {
				j.AddError(errors.Errorf("curator does not support uploading '%s' repos",
					j.Distro.Type))
				return
			}

			// do the sync. It's ok,
			err = b.SyncTo(ctx, syncSource, filepath.Join(remote, changedComponent), syncOpts)
			if err != nil {
				j.AddError(errors.Wrapf(err, "problem uploading %s to %s/%s",
					syncSource, b, changedComponent))
				return
			}
		}(remote, clonedBucket)
	}
	wg.Wait()

	grip.WarningWhen(j.HasErrors(), "encountered error rebuilding and uploading repositories. operation complete.")
	grip.NoticeWhen(!j.HasErrors(), "completed rebuilding all repositories")
}
