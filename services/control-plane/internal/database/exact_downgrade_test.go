package database

import (
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	migratedatabase "github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeExactVersion struct {
	version int
	dirty   bool
	err     error
}

type fakeExactSetVersion struct {
	version int
	dirty   bool
}

type fakeExactDatabase struct {
	mu sync.Mutex

	version int
	dirty   bool

	versionScript   []fakeExactVersion
	versionCalls    int
	versionLocked   []bool
	setVersionCalls []fakeExactSetVersion
	runBodies       []string
	lockCalls       int
	unlockCalls     int
	closeCalls      int
	dropCalls       int
	locked          bool

	lockErr     error
	unlockErr   error
	setDirtyErr error
	runErr      error
	setCleanErr error
	closeErr    error
}

func (f *fakeExactDatabase) Open(string) (migratedatabase.Driver, error) {
	return nil, errors.New("unexpected database Open call")
}

func (f *fakeExactDatabase) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	return f.closeErr
}

func (f *fakeExactDatabase) Lock() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lockCalls++
	if f.lockErr != nil {
		return f.lockErr
	}
	f.locked = true
	return nil
}

func (f *fakeExactDatabase) Unlock() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unlockCalls++
	if f.unlockErr != nil {
		return f.unlockErr
	}
	f.locked = false
	return nil
}

func (f *fakeExactDatabase) Run(reader io.Reader) error {
	body, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runBodies = append(f.runBodies, string(body))
	return f.runErr
}

func (f *fakeExactDatabase) SetVersion(version int, dirty bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setVersionCalls = append(f.setVersionCalls, fakeExactSetVersion{version: version, dirty: dirty})
	if dirty && f.setDirtyErr != nil {
		return f.setDirtyErr
	}
	if !dirty && f.setCleanErr != nil {
		return f.setCleanErr
	}
	f.version = version
	f.dirty = dirty
	return nil
}

func (f *fakeExactDatabase) Version() (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.versionLocked = append(f.versionLocked, f.locked)
	call := f.versionCalls
	f.versionCalls++
	if call < len(f.versionScript) {
		result := f.versionScript[call]
		return result.version, result.dirty, result.err
	}
	return f.version, f.dirty, nil
}

func (f *fakeExactDatabase) Drop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropCalls++
	return errors.New("unexpected database Drop call")
}

type fakeExactSource struct {
	mu sync.Mutex

	prevCalls     []uint
	readUpCalls   []uint
	readDownCalls []uint
	closeCalls    int
	prevErr       error
	readDownErr   error
	closeErr      error
}

func (f *fakeExactSource) Open(string) (source.Driver, error) {
	return nil, errors.New("unexpected source Open call")
}

func (f *fakeExactSource) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	return f.closeErr
}

func (f *fakeExactSource) First() (uint, error) {
	return 0, os.ErrNotExist
}

func (f *fakeExactSource) Prev(version uint) (uint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prevCalls = append(f.prevCalls, version)
	if f.prevErr != nil {
		return 0, f.prevErr
	}
	if version != 87 {
		return 0, os.ErrNotExist
	}
	return 86, nil
}

func (f *fakeExactSource) Next(uint) (uint, error) {
	return 0, errors.New("unexpected source Next call")
}

func (f *fakeExactSource) ReadUp(version uint) (io.ReadCloser, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readUpCalls = append(f.readUpCalls, version)
	if version != 87 {
		return nil, "", os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader("up migration 87")), "000087_presence_history.up.sql", nil
}

func (f *fakeExactSource) ReadDown(version uint) (io.ReadCloser, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readDownCalls = append(f.readDownCalls, version)
	if f.readDownErr != nil {
		return nil, "", f.readDownErr
	}
	return io.NopCloser(strings.NewReader("down migration 87")), "000087_presence_history.down.sql", nil
}

type exactDowngradeHarness struct {
	database    *fakeExactDatabase
	source      *fakeExactSource
	databaseURL string
	sourceURL   string
	deps        exactDowngradeDependencies
}

func newExactDowngradeHarness(version int, dirty bool) *exactDowngradeHarness {
	h := &exactDowngradeHarness{
		database: &fakeExactDatabase{version: version, dirty: dirty},
		source:   &fakeExactSource{},
	}
	h.deps = exactDowngradeDependencies{
		openDatabase: func(rawURL string) (migratedatabase.Driver, error) {
			h.databaseURL = rawURL
			return h.database, nil
		},
		openSource: func(rawURL string) (source.Driver, error) {
			h.sourceURL = rawURL
			return h.source, nil
		},
		newMigrate: newExactMigration,
	}
	return h
}

func TestExactDowngradeMigration87PublicAPI(t *testing.T) {
	downgrade := ExactDowngradeMigration87
	require.NotNil(t, downgrade)
}

func TestExactDowngradeMigration87RunsOneGuardedDownStep(t *testing.T) {
	h := newExactDowngradeHarness(87, false)

	err := runExactDowngradeMigration87("postgres://dedicated", h.deps)

	require.NoError(t, err)
	assert.Equal(t, "postgres://dedicated", h.databaseURL)
	assert.Equal(t, "file://migrations", h.sourceURL)
	assert.Equal(t, 1, h.database.lockCalls)
	assert.Equal(t, 1, h.database.unlockCalls)
	assert.Equal(t, []bool{true, false}, h.database.versionLocked,
		"the start guard must run under the migration lock and final verification must use the underlying driver")
	assert.Equal(t, []uint{87}, h.source.prevCalls)
	assert.Equal(t, []uint{87}, h.source.readUpCalls)
	assert.Equal(t, []uint{87}, h.source.readDownCalls)
	assert.Equal(t, []fakeExactSetVersion{{version: 86, dirty: true}, {version: 86, dirty: false}}, h.database.setVersionCalls)
	assert.Equal(t, []string{"down migration 87"}, h.database.runBodies)
	assert.Equal(t, 86, h.database.version)
	assert.False(t, h.database.dirty)
	assert.Equal(t, 0, h.database.dropCalls)
	assert.Equal(t, 1, h.source.closeCalls)
	assert.Equal(t, 1, h.database.closeCalls)
}

func TestExactDowngradeMigration87RejectsEveryNonExactStartStateUnderLock(t *testing.T) {
	tests := []struct {
		name    string
		version int
		dirty   bool
	}{
		{name: "version 86", version: 86},
		{name: "version 87 dirty", version: 87, dirty: true},
		{name: "version 88", version: 88},
		{name: "nil version", version: migratedatabase.NilVersion},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newExactDowngradeHarness(tt.version, tt.dirty)

			err := runExactDowngradeMigration87("postgres://dedicated", h.deps)

			require.Error(t, err)
			assert.ErrorIs(t, err, errExactDowngradeStartState)
			assert.Equal(t, []bool{true}, h.database.versionLocked)
			assert.Empty(t, h.database.setVersionCalls)
			assert.Empty(t, h.database.runBodies)
			assert.Empty(t, h.source.prevCalls)
			assert.Equal(t, 1, h.database.unlockCalls)
			assert.Equal(t, 1, h.source.closeCalls)
			assert.Equal(t, 1, h.database.closeCalls)
		})
	}
}

func TestExactDowngradeMigration87PropagatesStepErrors(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*fakeExactDatabase, *fakeExactSource, error)
	}{
		{
			name: "lock acquisition",
			configure: func(database *fakeExactDatabase, _ *fakeExactSource, want error) {
				database.lockErr = want
			},
		},
		{
			name: "locked version read",
			configure: func(database *fakeExactDatabase, _ *fakeExactSource, want error) {
				database.versionScript = []fakeExactVersion{{err: want}}
			},
		},
		{
			name: "source previous version",
			configure: func(_ *fakeExactDatabase, migrationSource *fakeExactSource, want error) {
				migrationSource.prevErr = want
			},
		},
		{
			name: "source read down",
			configure: func(_ *fakeExactDatabase, migrationSource *fakeExactSource, want error) {
				migrationSource.readDownErr = want
			},
		},
		{
			name: "set dirty version",
			configure: func(database *fakeExactDatabase, _ *fakeExactSource, want error) {
				database.setDirtyErr = want
			},
		},
		{
			name: "run down migration",
			configure: func(database *fakeExactDatabase, _ *fakeExactSource, want error) {
				database.runErr = want
			},
		},
		{
			name: "set clean version",
			configure: func(database *fakeExactDatabase, _ *fakeExactSource, want error) {
				database.setCleanErr = want
			},
		},
		{
			name: "unlock",
			configure: func(database *fakeExactDatabase, _ *fakeExactSource, want error) {
				database.unlockErr = want
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newExactDowngradeHarness(87, false)
			want := errors.New("step failure")
			tt.configure(h.database, h.source, want)

			err := runExactDowngradeMigration87("postgres://dedicated", h.deps)

			require.Error(t, err)
			assert.ErrorIs(t, err, want)
			assert.Equal(t, 1, h.source.closeCalls)
			assert.Equal(t, 1, h.database.closeCalls)
		})
	}
}

func TestExactDowngradeMigration87VerifiesUnderlyingCleanVersion86(t *testing.T) {
	verifyErr := errors.New("verify version failure")
	tests := []struct {
		name   string
		result fakeExactVersion
	}{
		{name: "wrong final version", result: fakeExactVersion{version: 87}},
		{name: "dirty final version", result: fakeExactVersion{version: 86, dirty: true}},
		{name: "final version read error", result: fakeExactVersion{err: verifyErr}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newExactDowngradeHarness(87, false)
			h.database.versionScript = []fakeExactVersion{{version: 87}, tt.result}

			err := runExactDowngradeMigration87("postgres://dedicated", h.deps)

			require.Error(t, err)
			assert.ErrorIs(t, err, errExactDowngradeFinalState)
			if tt.result.err != nil {
				assert.ErrorIs(t, err, verifyErr)
			}
			assert.Equal(t, []bool{true, false}, h.database.versionLocked)
			assert.Len(t, h.database.runBodies, 1)
		})
	}
}

func TestExactDowngradeMigration87JoinsStepAndCloseErrors(t *testing.T) {
	stepErr := errors.New("step failure")
	sourceCloseErr := errors.New("source close failure")
	databaseCloseErr := errors.New("database close failure")
	tests := []struct {
		name             string
		stepErr          error
		sourceCloseErr   error
		databaseCloseErr error
	}{
		{name: "source close", sourceCloseErr: sourceCloseErr},
		{name: "database close", databaseCloseErr: databaseCloseErr},
		{name: "both closes", sourceCloseErr: sourceCloseErr, databaseCloseErr: databaseCloseErr},
		{name: "step and both closes", stepErr: stepErr, sourceCloseErr: sourceCloseErr, databaseCloseErr: databaseCloseErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newExactDowngradeHarness(87, false)
			h.database.runErr = tt.stepErr
			h.source.closeErr = tt.sourceCloseErr
			h.database.closeErr = tt.databaseCloseErr

			err := runExactDowngradeMigration87("postgres://dedicated", h.deps)

			require.Error(t, err)
			for _, want := range []error{tt.stepErr, tt.sourceCloseErr, tt.databaseCloseErr} {
				if want != nil {
					assert.ErrorIs(t, err, want)
				}
			}
			assert.Equal(t, 1, h.source.closeCalls)
			assert.Equal(t, 1, h.database.closeCalls)
		})
	}
}

func TestExactDowngradeMigration87ClosesPartialConstruction(t *testing.T) {
	t.Run("database open failure owns nothing", func(t *testing.T) {
		openErr := errors.New("database open failure")
		sourceOpened := false
		deps := exactDowngradeDependencies{
			openDatabase: func(string) (migratedatabase.Driver, error) { return nil, openErr },
			openSource: func(string) (source.Driver, error) {
				sourceOpened = true
				return nil, nil
			},
		}

		err := runExactDowngradeMigration87("postgres://dedicated", deps)

		assert.ErrorIs(t, err, openErr)
		assert.False(t, sourceOpened)
	})

	t.Run("source open failure closes database", func(t *testing.T) {
		openErr := errors.New("source open failure")
		closeErr := errors.New("database close failure")
		database := &fakeExactDatabase{closeErr: closeErr}
		deps := exactDowngradeDependencies{
			openDatabase: func(string) (migratedatabase.Driver, error) { return database, nil },
			openSource:   func(string) (source.Driver, error) { return nil, openErr },
		}

		err := runExactDowngradeMigration87("postgres://dedicated", deps)

		assert.ErrorIs(t, err, openErr)
		assert.ErrorIs(t, err, closeErr)
		assert.Equal(t, 1, database.closeCalls)
	})

	t.Run("migrate construction failure closes source and database", func(t *testing.T) {
		constructErr := errors.New("migrate construction failure")
		sourceCloseErr := errors.New("source close failure")
		databaseCloseErr := errors.New("database close failure")
		database := &fakeExactDatabase{closeErr: databaseCloseErr}
		migrationSource := &fakeExactSource{closeErr: sourceCloseErr}
		deps := exactDowngradeDependencies{
			openDatabase: func(string) (migratedatabase.Driver, error) { return database, nil },
			openSource:   func(string) (source.Driver, error) { return migrationSource, nil },
			newMigrate: func(source.Driver, migratedatabase.Driver) (exactMigration, error) {
				return nil, constructErr
			},
		}

		err := runExactDowngradeMigration87("postgres://dedicated", deps)

		assert.ErrorIs(t, err, constructErr)
		assert.ErrorIs(t, err, sourceCloseErr)
		assert.ErrorIs(t, err, databaseCloseErr)
		assert.Equal(t, 1, migrationSource.closeCalls)
		assert.Equal(t, 1, database.closeCalls)
	})
}
