// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package mydump

import (
	"context"
	"path/filepath"
	"sort"

	"github.com/pingcap/br/pkg/storage"
	"github.com/pingcap/errors"
	filter "github.com/pingcap/tidb-tools/pkg/table-filter"
	router "github.com/pingcap/tidb-tools/pkg/table-router"
	"go.uber.org/zap"

	"github.com/pingcap/tidb-lightning/lightning/config"
	"github.com/pingcap/tidb-lightning/lightning/log"
)

type MDDatabaseMeta struct {
	Name       string
	SchemaFile string
	Tables     []*MDTableMeta
	charSet    string
}

type MDTableMeta struct {
	DB         string
	Name       string
	SchemaFile FileInfo
	DataFiles  []FileInfo
	charSet    string
	TotalSize  int64
}

type SourceFileMeta struct {
	Path        string
	Type        SourceType
	Compression Compression
	SortKey     string
}

func (m *MDTableMeta) GetSchema(ctx context.Context, store storage.ExternalStorage) string {
	schema, err := ExportStatement(ctx, store, m.SchemaFile, m.charSet)
	if err != nil {
		log.L().Error("failed to extract table schema",
			zap.String("Path", m.SchemaFile.FileMeta.Path),
			log.ShortError(err),
		)
		return ""
	}
	return string(schema)
}

/*
	Mydumper File Loader
*/
type MDLoader struct {
	store      storage.ExternalStorage
	noSchema   bool
	dbs        []*MDDatabaseMeta
	filter     filter.Filter
	router     *router.Table
	fileRouter FileRouter
	charSet    string
}

type mdLoaderSetup struct {
	loader        *MDLoader
	dbSchemas     []FileInfo
	tableSchemas  []FileInfo
	tableDatas    []FileInfo
	dbIndexMap    map[string]int
	tableIndexMap map[filter.Table]int
}

func NewMyDumpLoader(ctx context.Context, cfg *config.Config) (*MDLoader, error) {
	u, err := storage.ParseBackend(cfg.Mydumper.SourceDir, nil)
	if err != nil {
		return nil, err
	}
	s, err := storage.Create(ctx, u, true)
	if err != nil {
		return nil, err
	}

	return NewMyDumpLoaderWithStore(ctx, cfg, s)
}

func NewMyDumpLoaderWithStore(ctx context.Context, cfg *config.Config, store storage.ExternalStorage) (*MDLoader, error) {
	var r *router.Table
	var err error

	if len(cfg.Routes) > 0 && len(cfg.Mydumper.FileRouters) > 0 {
		return nil, errors.New("table route is deprecated, can't config both [routes] and [mydumper.files]")
	}

	if len(cfg.Routes) > 0 {
		r, err = router.NewTableRouter(cfg.Mydumper.CaseSensitive, cfg.Routes)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	// use the legacy black-white-list if defined. otherwise use the new filter.
	var f filter.Filter
	if cfg.HasLegacyBlackWhiteList() {
		f, err = filter.ParseMySQLReplicationRules(&cfg.BWList)
	} else {
		f, err = filter.Parse(cfg.Mydumper.Filter)
	}
	if err != nil {
		return nil, err
	}
	if !cfg.Mydumper.CaseSensitive {
		f = filter.CaseInsensitive(f)
	}

	fileRouteRules := cfg.Mydumper.FileRouters
	if cfg.Mydumper.DefaultFileRules {
		fileRouteRules = append(fileRouteRules, defaultFileRouteRules...)
	}

	fileRouter, err := NewFileRouter(fileRouteRules)
	if err != nil {
		return nil, err
	}

	mdl := &MDLoader{
		store:      store,
		noSchema:   cfg.Mydumper.NoSchema,
		filter:     f,
		router:     r,
		charSet:    cfg.Mydumper.CharacterSet,
		fileRouter: fileRouter,
	}

	setup := mdLoaderSetup{
		loader:        mdl,
		dbIndexMap:    make(map[string]int),
		tableIndexMap: make(map[filter.Table]int),
	}

	if err := setup.setup(ctx, mdl.store); err != nil {
		return nil, errors.Trace(err)
	}

	return mdl, nil
}

type fileType int

const (
	fileTypeDatabaseSchema fileType = iota
	fileTypeTableSchema
	fileTypeTableData
)

func (ftype fileType) String() string {
	switch ftype {
	case fileTypeDatabaseSchema:
		return "database schema"
	case fileTypeTableSchema:
		return "table schema"
	case fileTypeTableData:
		return "table data"
	default:
		return "(unknown)"
	}
}

type FileInfo struct {
	TableName filter.Table
	FileMeta  SourceFileMeta
	Size      int64
}

// setup the `s.loader.dbs` slice by scanning all *.sql files inside `dir`.
//
// The database and tables are inserted in a consistent order, so creating an
// MDLoader twice with the same data source is going to produce the same array,
// even after killing Lightning.
//
// This is achieved by using `filepath.Walk` internally which guarantees the
// files are visited in lexicographical order (note that this does not mean the
// databases and tables in the end are ordered lexicographically since they may
// be stored in different subdirectories).
//
// Will sort tables by table size, this means that the big table is imported
// at the latest, which to avoid large table take a long time to import and block
// small table to release index worker.
func (s *mdLoaderSetup) setup(ctx context.Context, store storage.ExternalStorage) error {
	/*
		Mydumper file names format
			db    —— {db}-schema-create.sql
			table —— {db}.{table}-schema.sql
			sql   —— {db}.{table}.{part}.sql / {db}.{table}.sql
	*/
	if err := s.listFiles(ctx, store); err != nil {
		return errors.Annotate(err, "list file failed")
	}
	if err := s.route(); err != nil {
		return errors.Trace(err)
	}

	if !s.loader.noSchema {
		// setup database schema
		if len(s.dbSchemas) == 0 {
			return errors.New("missing {schema}-schema-create.sql")
		}
		for _, fileInfo := range s.dbSchemas {
			if _, dbExists := s.insertDB(fileInfo.TableName.Schema, fileInfo.FileMeta.Path); dbExists && s.loader.router == nil {
				return errors.Errorf("invalid database schema file, duplicated item - %s", fileInfo.FileMeta.Path)
			}
		}

		// setup table schema
		for _, fileInfo := range s.tableSchemas {
			_, dbExists, tableExists := s.insertTable(fileInfo)
			if !dbExists {
				return errors.Errorf("invalid table schema file, cannot find db '%s' - %s", fileInfo.TableName.Schema, fileInfo.FileMeta.Path)
			} else if tableExists && s.loader.router == nil {
				return errors.Errorf("invalid table schema file, duplicated item - %s", fileInfo.FileMeta.Path)
			}
		}
	}

	// Sql file for restore data
	for _, fileInfo := range s.tableDatas {
		// set a dummy `FileInfo` here without file meta because we needn't restore the table schema
		tableMeta, dbExists, tableExists := s.insertTable(FileInfo{TableName: fileInfo.TableName})
		if !s.loader.noSchema {
			if !dbExists {
				return errors.Errorf("invalid data file, miss host db '%s' - %s", fileInfo.TableName.Schema, fileInfo.FileMeta.Path)
			} else if !tableExists {
				return errors.Errorf("invalid data file, miss host table '%s' - %s", fileInfo.TableName.Name, fileInfo.FileMeta.Path)
			}
		}
		tableMeta.DataFiles = append(tableMeta.DataFiles, fileInfo)
		tableMeta.TotalSize += fileInfo.Size
	}

	for _, dbMeta := range s.loader.dbs {
		// Put the small table in the front of the slice which can avoid large table
		// take a long time to import and block small table to release index worker.
		sort.SliceStable(dbMeta.Tables, func(i, j int) bool {
			return dbMeta.Tables[i].TotalSize < dbMeta.Tables[j].TotalSize
		})

		// sort each table source files by sort-key
		for _, tbMeta := range dbMeta.Tables {
			dataFiles := tbMeta.DataFiles
			sort.SliceStable(dataFiles, func(i, j int) bool {
				return dataFiles[i].FileMeta.SortKey < dataFiles[j].FileMeta.SortKey
			})
		}
	}

	return nil
}

func (s *mdLoaderSetup) listFiles(ctx context.Context, store storage.ExternalStorage) error {
	// `filepath.Walk` yields the paths in a deterministic (lexicographical) order,
	// meaning the file and chunk orders will be the same everytime it is called
	// (as long as the source is immutable).
	err := store.WalkDir(ctx, &storage.WalkOption{}, func(path string, size int64) error {
		logger := log.With(zap.String("path", path))

		res, err := s.loader.fileRouter.Route(filepath.ToSlash(path))
		if err != nil {
			return errors.Annotatef(err, "apply file routing on file '%s' failed", path)
		}
		if res == nil {
			logger.Info("[loader] file is filtered by file router")
			return nil
		}

		info := FileInfo{
			TableName: filter.Table{Schema: res.Schema, Name: res.Name},
			FileMeta:  SourceFileMeta{Path: path, Type: res.Type, Compression: res.Compression, SortKey: res.Key},
			Size:      size,
		}

		if s.loader.shouldSkip(&info.TableName) {
			logger.Debug("[filter] ignoring table file")

			return nil
		}

		switch res.Type {
		case SourceTypeSchemaSchema:
			s.dbSchemas = append(s.dbSchemas, info)
		case SourceTypeTableSchema:
			s.tableSchemas = append(s.tableSchemas, info)
		case SourceTypeSQL, SourceTypeCSV, SourceTypeParquet:
			s.tableDatas = append(s.tableDatas, info)
		}

		logger.Info("file route result", zap.String("schema", res.Schema),
			zap.String("table", res.Name), zap.Stringer("type", res.Type))

		return nil
	})

	return errors.Trace(err)
}

func (l *MDLoader) shouldSkip(table *filter.Table) bool {
	if len(table.Name) == 0 {
		return !l.filter.MatchSchema(table.Schema)
	}
	return !l.filter.MatchTable(table.Schema, table.Name)
}

func (s *mdLoaderSetup) route() error {
	r := s.loader.router
	if r == nil {
		return nil
	}

	type dbInfo struct {
		fileMeta SourceFileMeta
		count    int
	}

	knownDBNames := make(map[string]dbInfo)
	for _, info := range s.dbSchemas {
		knownDBNames[info.TableName.Schema] = dbInfo{
			fileMeta: info.FileMeta,
			count:    1,
		}
	}
	for _, info := range s.tableSchemas {
		dbInfo := knownDBNames[info.TableName.Schema]
		dbInfo.count++
		knownDBNames[info.TableName.Schema] = dbInfo
	}

	run := func(arr []FileInfo) error {
		for i, info := range arr {
			dbName, tableName, err := r.Route(info.TableName.Schema, info.TableName.Name)
			if err != nil {
				return errors.Trace(err)
			}
			if dbName != info.TableName.Schema {
				oldInfo := knownDBNames[info.TableName.Schema]
				oldInfo.count--
				knownDBNames[info.TableName.Schema] = oldInfo

				newInfo, ok := knownDBNames[dbName]
				newInfo.count++
				if !ok {
					newInfo.fileMeta = oldInfo.fileMeta
					s.dbSchemas = append(s.dbSchemas, FileInfo{
						TableName: filter.Table{Schema: dbName},
						FileMeta:  oldInfo.fileMeta,
					})
				}
				knownDBNames[dbName] = newInfo
			}
			arr[i].TableName = filter.Table{Schema: dbName, Name: tableName}
		}
		return nil
	}

	if err := run(s.tableSchemas); err != nil {
		return errors.Trace(err)
	}
	if err := run(s.tableDatas); err != nil {
		return errors.Trace(err)
	}

	// remove all schemas which has been entirely routed away
	// https://github.com/golang/go/wiki/SliceTricks#filtering-without-allocating
	remainingSchemas := s.dbSchemas[:0]
	for _, info := range s.dbSchemas {
		if knownDBNames[info.TableName.Schema].count > 0 {
			remainingSchemas = append(remainingSchemas, info)
		}
	}
	s.dbSchemas = remainingSchemas

	return nil
}

func (s *mdLoaderSetup) insertDB(dbName string, path string) (*MDDatabaseMeta, bool) {
	dbIndex, ok := s.dbIndexMap[dbName]
	if ok {
		return s.loader.dbs[dbIndex], true
	} else {
		s.dbIndexMap[dbName] = len(s.loader.dbs)
		ptr := &MDDatabaseMeta{
			Name:       dbName,
			SchemaFile: path,
			charSet:    s.loader.charSet,
		}
		s.loader.dbs = append(s.loader.dbs, ptr)
		return ptr, false
	}
}

func (s *mdLoaderSetup) insertTable(fileInfo FileInfo) (*MDTableMeta, bool, bool) {
	dbMeta, dbExists := s.insertDB(fileInfo.TableName.Schema, "")
	tableIndex, ok := s.tableIndexMap[fileInfo.TableName]
	if ok {
		return dbMeta.Tables[tableIndex], dbExists, true
	} else {
		s.tableIndexMap[fileInfo.TableName] = len(dbMeta.Tables)
		ptr := &MDTableMeta{
			DB:         fileInfo.TableName.Schema,
			Name:       fileInfo.TableName.Name,
			SchemaFile: fileInfo,
			DataFiles:  make([]FileInfo, 0, 16),
			charSet:    s.loader.charSet,
		}
		dbMeta.Tables = append(dbMeta.Tables, ptr)
		return ptr, dbExists, false
	}
}

func (l *MDLoader) GetDatabases() []*MDDatabaseMeta {
	return l.dbs
}

func (l *MDLoader) GetStore() storage.ExternalStorage {
	return l.store
}
