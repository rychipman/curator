package repobuilder

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/mongodb/amboy"
	"github.com/mongodb/amboy/dependency"
	"github.com/tychoish/grip"
)

// JobBase provides basic functionality used by the
// RepoBuilder job implementation and IndexBuilder Jobs. This is an
// internal type, but must be public because it's part of the exported
// Jobs interface and contains data that would need to be serialized
// with the job object.
type JobBase struct {
	Name       string             `bson:"name" json:"name" yaml:"name"`
	IsComplete bool               `bson:"completed" json:"completed" yaml:"completed"`
	JobType    amboy.JobType      `bson:"job_type" json:"job_type" yaml:"job_type"`
	D          dependency.Manager `bson:"dependency" json:"dependency" yaml:"dependency"`
	Errors     []error            `bson:"errors" json:"errors" yaml:"errors"`
	mutex      sync.RWMutex
}

// ID returns the name of the job, and is a component of the amboy.Job
// interface.
func (j *JobBase) ID() string {
	return j.Name
}

// Completed returns true if the job has been marked completed, and is
// a component of the amboy.Job interface.
func (j *JobBase) Completed() bool {
	j.mutex.RLock()
	defer j.mutex.RUnlock()

	return j.IsComplete
}

// Type returns the amboy.JobType specification for this object, and
// is a component of the amboy.Job interface.
func (j *JobBase) Type() amboy.JobType {
	return j.JobType
}

// Dependency returns an amboy Job dependency interface object, and is
// a component of the amboy.Job interface.
func (j *JobBase) Dependency() dependency.Manager {
	j.mutex.RLock()
	defer j.mutex.RUnlock()

	return j.D
}

// SetDependency allows you to inject a different amboy.Job dependency
// object, and is a component of the amboy.Job interface.
func (j *JobBase) SetDependency(d dependency.Manager) {
	if d.Type().Name == dependency.AlwaysRun {
		j.mutex.Lock()
		defer j.mutex.Unlock()

		j.D = d
	} else {
		grip.Warningln("repo must have 'always'-run dependencies.",
			"not setting dependency to:", d.Type().Name)
	}
}

func (j *JobBase) markComplete() {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	j.IsComplete = true
}

func (j *JobBase) addError(err error) {
	if err != nil {
		j.mutex.Lock()
		defer j.mutex.Unlock()

		j.Errors = append(j.Errors, err)
	}
}

func (j *JobBase) hasErrors() bool {
	j.mutex.RLock()
	defer j.mutex.RUnlock()

	return len(j.Errors) > 0
}

func (j *JobBase) Error() error {
	j.mutex.RLock()
	defer j.mutex.RUnlock()

	if len(j.Errors) == 0 {
		return nil
	}

	var outputs []string

	for _, err := range j.Errors {
		outputs = append(outputs, fmt.Sprintf("%+v", err))
	}

	return errors.New(strings.Join(outputs, "\n"))
}