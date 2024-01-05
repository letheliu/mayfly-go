package dbm

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/sync/singleflight"

	"mayfly-go/internal/db/config"
	"mayfly-go/internal/db/domain/entity"
	"mayfly-go/pkg/logx"
)

var _ DbProgram = (*DbProgramMysql)(nil)

type DbProgramMysql struct {
	dbConn *DbConn
	// mysqlBin 用于集成测试
	mysqlBin *config.MysqlBin
	// backupPath 用于集成测试
	backupPath string
}

func NewDbProgramMysql(dbConn *DbConn) *DbProgramMysql {
	return &DbProgramMysql{
		dbConn: dbConn,
	}
}

func (svc *DbProgramMysql) dbInfo() *DbInfo {
	return svc.dbConn.Info
}

func (svc *DbProgramMysql) getMysqlBin() *config.MysqlBin {
	if svc.mysqlBin != nil {
		return svc.mysqlBin
	}
	var mysqlBin *config.MysqlBin
	switch svc.dbInfo().Type {
	case DbTypeMariadb:
		mysqlBin = config.GetMysqlBin(config.ConfigKeyDbMariadbBin)
	case DbTypeMysql:
		mysqlBin = config.GetMysqlBin(config.ConfigKeyDbMysqlBin)
	default:
		panic(fmt.Sprintf("不兼容 MySQL 的数据库类型: %v", svc.dbInfo().Type))
	}
	svc.mysqlBin = mysqlBin
	return svc.mysqlBin
}

func (svc *DbProgramMysql) getBackupPath() string {
	if len(svc.backupPath) > 0 {
		return svc.backupPath
	}
	return config.GetDbBackupRestore().BackupPath
}

func (svc *DbProgramMysql) GetBinlogFilePath(fileName string) string {
	return filepath.Join(svc.getBinlogDir(svc.dbInfo().InstanceId), fileName)
}

func (svc *DbProgramMysql) Backup(ctx context.Context, backupHistory *entity.DbBackupHistory) (*entity.BinlogInfo, error) {
	dir := svc.getDbBackupDir(backupHistory.DbInstanceId, backupHistory.DbBackupId)
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return nil, err
	}
	tmpFile := filepath.Join(dir, "backup.tmp")
	defer func() {
		_ = os.Remove(tmpFile)
	}()

	args := []string{
		"--host", svc.dbInfo().Host,
		"--port", strconv.Itoa(svc.dbInfo().Port),
		"--user", svc.dbInfo().Username,
		"--password=" + svc.dbInfo().Password,
		"--add-drop-database",
		"--result-file", tmpFile,
		"--single-transaction",
		"--master-data=2",
		"--databases", backupHistory.DbName,
	}

	cmd := exec.CommandContext(ctx, svc.getMysqlBin().MysqldumpPath, args...)
	logx.Debugf("backup database using mysqldump binary: %s", cmd.String())
	if err := runCmd(cmd); err != nil {
		logx.Errorf("运行 mysqldump 程序失败: %v", err)
		return nil, errors.Wrap(err, "运行 mysqldump 程序失败")
	}

	logx.Debugf("Checking dumped file stat: %s", tmpFile)
	if _, err := os.Stat(tmpFile); err != nil {
		logx.Errorf("未找到备份文件: %v", err)
		return nil, errors.Wrapf(err, "未找到备份文件")
	}
	reader, err := os.Open(tmpFile)
	if err != nil {
		return nil, err
	}
	binlogInfo, err := readBinlogInfoFromBackup(reader)
	_ = reader.Close()
	if err != nil {
		return nil, errors.Wrapf(err, "从备份文件中读取 binlog 信息失败")
	}
	fileName := filepath.Join(dir, fmt.Sprintf("%s.sql", backupHistory.Uuid))
	if err := os.Rename(tmpFile, fileName); err != nil {
		return nil, errors.Wrap(err, "备份文件改名失败")
	}

	return binlogInfo, nil
}

func (svc *DbProgramMysql) RestoreBackupHistory(ctx context.Context, dbName string, dbBackupId uint64, dbBackupHistoryUuid string) error {
	args := []string{
		"--host", svc.dbInfo().Host,
		"--port", strconv.Itoa(svc.dbInfo().Port),
		"--database", dbName,
		"--user", svc.dbInfo().Username,
		"--password=" + svc.dbInfo().Password,
	}

	fileName := filepath.Join(svc.getDbBackupDir(svc.dbInfo().InstanceId, dbBackupId),
		fmt.Sprintf("%v.sql", dbBackupHistoryUuid))
	file, err := os.Open(fileName)
	if err != nil {
		return errors.Wrap(err, "打开备份文件失败")
	}
	defer func() {
		_ = file.Close()
	}()

	cmd := exec.CommandContext(ctx, svc.getMysqlBin().MysqlPath, args...)
	cmd.Stdin = file
	logx.Debug("恢复数据库: ", cmd.String())
	if err := runCmd(cmd); err != nil {
		logx.Errorf("运行 mysql 程序失败: %v", err)
		return errors.Wrap(err, "运行 mysql 程序失败")
	}
	return nil
}

// Download binlog files on server.
func (svc *DbProgramMysql) downloadBinlogFilesOnServer(ctx context.Context, binlogFilesOnServerSorted []*entity.BinlogFile, downloadLatestBinlogFile bool) error {
	if len(binlogFilesOnServerSorted) == 0 {
		logx.Debug("No binlog file found on server to download")
		return nil
	}
	if err := os.MkdirAll(svc.getBinlogDir(svc.dbInfo().InstanceId), os.ModePerm); err != nil {
		return errors.Wrapf(err, "创建 binlog 目录失败: %q", svc.getBinlogDir(svc.dbInfo().InstanceId))
	}
	latestBinlogFileOnServer := binlogFilesOnServerSorted[len(binlogFilesOnServerSorted)-1]
	for _, fileOnServer := range binlogFilesOnServerSorted {
		isLatest := fileOnServer.Name == latestBinlogFileOnServer.Name
		if isLatest && !downloadLatestBinlogFile {
			continue
		}
		binlogFilePath := filepath.Join(svc.getBinlogDir(svc.dbInfo().InstanceId), fileOnServer.Name)
		logx.Debug("Downloading binlog file from MySQL server.", logx.String("path", binlogFilePath), logx.Bool("isLatest", isLatest))
		if err := svc.downloadBinlogFile(ctx, fileOnServer, isLatest); err != nil {
			logx.Error("下载 binlog 文件失败", logx.String("path", binlogFilePath), logx.String("error", err.Error()))
			return errors.Wrapf(err, "下载 binlog 文件失败: %q", binlogFilePath)
		}
	}
	return nil
}

// Parse the first binlog eventTs of a local binlog file.
func (svc *DbProgramMysql) parseLocalBinlogFirstEventTime(ctx context.Context, filePath string) (eventTime time.Time, parseErr error) {
	args := []string{
		// Local binlog file path.
		filePath,
		// Verify checksum binlog events.
		"--verify-binlog-checksum",
		// Tell mysqlbinlog to suppress the BINLOG statements for row events, which reduces the unneeded output.
		"--base64-output=DECODE-ROWS",
	}
	cmd := exec.CommandContext(ctx, svc.getMysqlBin().MysqlbinlogPath, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	pr, err := cmd.StdoutPipe()
	if err != nil {
		return time.Time{}, err
	}

	if err := cmd.Start(); err != nil {
		return time.Time{}, err
	}
	defer func() {
		_ = cmd.Cancel()
		if err := cmd.Wait(); err != nil && parseErr != nil && stderr.Len() > 0 {
			parseErr = errors.Wrap(parseErr, stderr.String())
		}
	}()

	for s := bufio.NewScanner(pr); s.Scan(); {
		line := s.Text()
		eventTimeParsed, found, err := parseBinlogEventTimeInLine(line)
		if err != nil {
			return time.Time{}, errors.Wrap(err, "解析 binlog 文件失败")
		}
		if found {
			return eventTimeParsed, nil
		}
	}
	return time.Time{}, errors.New("解析 binlog 文件失败")
}

var singleFlightGroup singleflight.Group

// FetchBinlogs downloads binlog files from startingFileName on server to `binlogDir`.
func (svc *DbProgramMysql) FetchBinlogs(ctx context.Context, downloadLatestBinlogFile bool, earliestBackupSequence, latestBinlogSequence int64) ([]*entity.BinlogFile, error) {
	var downloaded bool
	key := strconv.FormatUint(svc.dbInfo().InstanceId, 16)
	binlogFiles, err, _ := singleFlightGroup.Do(key, func() (interface{}, error) {
		downloaded = true
		return svc.fetchBinlogs(ctx, downloadLatestBinlogFile, earliestBackupSequence, latestBinlogSequence)
	})
	if err != nil {
		return nil, err
	}
	if downloaded {
		return binlogFiles.([]*entity.BinlogFile), nil
	}
	if !downloadLatestBinlogFile {
		return nil, nil
	}
	binlogFiles, err, _ = singleFlightGroup.Do(key, func() (interface{}, error) {
		return svc.fetchBinlogs(ctx, true, earliestBackupSequence, latestBinlogSequence)
	})
	if err != nil {
		return nil, err
	}
	return binlogFiles.([]*entity.BinlogFile), err
}

// fetchBinlogs downloads binlog files from startingFileName on server to `binlogDir`.
func (svc *DbProgramMysql) fetchBinlogs(ctx context.Context, downloadLatestBinlogFile bool, earliestBackupSequence, latestBinlogSequence int64) ([]*entity.BinlogFile, error) {
	// Read binlog files list on server.
	binlogFilesOnServerSorted, err := svc.GetSortedBinlogFilesOnServer(ctx)
	if err != nil {
		return nil, err
	}
	if len(binlogFilesOnServerSorted) == 0 {
		logx.Debug("No binlog file found on server to download")
		return nil, nil
	}
	indexHistory := -1
	for i, file := range binlogFilesOnServerSorted {
		if latestBinlogSequence == file.Sequence {
			indexHistory = i + 1
			break
		}
		if earliestBackupSequence == file.Sequence {
			indexHistory = i
			break
		}
	}
	if indexHistory < 0 {
		return nil, errors.New(fmt.Sprintf("在数据库服务器上未找到 binlog 文件: %d, %d", earliestBackupSequence, latestBinlogSequence))
	}
	if indexHistory > len(binlogFilesOnServerSorted)-1 {
		indexHistory = len(binlogFilesOnServerSorted) - 1
	}
	binlogFilesOnServerSorted = binlogFilesOnServerSorted[indexHistory:]

	if err := svc.downloadBinlogFilesOnServer(ctx, binlogFilesOnServerSorted, downloadLatestBinlogFile); err != nil {
		return nil, err
	}

	return binlogFilesOnServerSorted, nil
}

// Syncs the binlog specified by `meta` between the instance and local.
// If isLast is true, it means that this is the last binlog file containing the targetTs event.
// It may keep growing as there are ongoing writes to the database. So we just need to check that
// the file size is larger or equal to the binlog file size we queried from the MySQL server earlier.
func (svc *DbProgramMysql) downloadBinlogFile(ctx context.Context, binlogFileToDownload *entity.BinlogFile, isLast bool) error {
	tempBinlogPrefix := filepath.Join(svc.getBinlogDir(svc.dbInfo().InstanceId), "tmp-")
	args := []string{
		binlogFileToDownload.Name,
		"--read-from-remote-server",
		// Verify checksum binlog events.
		"--verify-binlog-checksum",
		"--host", svc.dbInfo().Host,
		"--port", strconv.Itoa(svc.dbInfo().Port),
		"--user", svc.dbInfo().Username,
		"--raw",
		// With --raw this is a prefix for the file names.
		"--result-file", tempBinlogPrefix,
	}

	cmd := exec.CommandContext(ctx, svc.getMysqlBin().MysqlbinlogPath, args...)
	// We cannot set password as a flag. Otherwise, there is warning message
	// "mysqlbinlog: [Warning] Using a password on the command line interface can be insecure."
	if svc.dbInfo().Password != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("MYSQL_PWD=%s", svc.dbInfo().Password))
	}

	logx.Debug("Downloading binlog files using mysqlbinlog:", cmd.String())
	binlogFilePathTemp := tempBinlogPrefix + binlogFileToDownload.Name
	defer func() {
		_ = os.Remove(binlogFilePathTemp)
	}()
	if err := runCmd(cmd); err != nil {
		logx.Errorf("运行 mysqlbinlog 程序失败: %v", err)
		return errors.Wrap(err, "运行 mysqlbinlog 程序失败")
	}

	logx.Debug("Checking downloaded binlog file stat", logx.String("path", binlogFilePathTemp))
	binlogFileTempInfo, err := os.Stat(binlogFilePathTemp)
	if err != nil {
		logx.Error("未找到 binlog 文件", logx.String("path", binlogFilePathTemp), logx.String("error", err.Error()))
		return errors.Wrapf(err, "未找到 binlog 文件: %q", binlogFilePathTemp)
	}
	if !isLast && binlogFileTempInfo.Size() != binlogFileToDownload.Size {
		logx.Error("Downloaded archived binlog file size is not equal to size queried on the MySQL server earlier.",
			logx.String("binlog", binlogFileToDownload.Name),
			logx.Int64("sizeInfo", binlogFileToDownload.Size),
			logx.Int64("downloadedSize", binlogFileTempInfo.Size()),
		)
		return errors.Errorf("下载的 binlog 文件 %q 与服务上的文件大小不一致 %d != %d", binlogFilePathTemp, binlogFileTempInfo.Size(), binlogFileToDownload.Size)
	}

	binlogFilePath := svc.GetBinlogFilePath(binlogFileToDownload.Name)
	if err := os.Rename(binlogFilePathTemp, binlogFilePath); err != nil {
		return errors.Wrapf(err, "binlog 文件更名失败: %q -> %q", binlogFilePathTemp, binlogFilePath)
	}
	firstEventTime, err := svc.parseLocalBinlogFirstEventTime(ctx, binlogFilePath)
	if err != nil {
		return err
	}
	binlogFileToDownload.FirstEventTime = firstEventTime
	binlogFileToDownload.Downloaded = true

	return nil
}

// GetSortedBinlogFilesOnServer returns the information of binlog files in ascending order by their numeric extension.
func (svc *DbProgramMysql) GetSortedBinlogFilesOnServer(_ context.Context) ([]*entity.BinlogFile, error) {
	query := "SHOW BINARY LOGS"
	columns, rows, err := svc.dbConn.Query(query)
	if err != nil {
		return nil, errors.Wrapf(err, "SQL 语句 %q 执行失败", query)
	}
	findFileName := false
	findFileSize := false
	for _, column := range columns {
		switch column.Name {
		case "Log_name":
			findFileName = true
		case "File_size":
			findFileSize = true
		}
	}
	if !findFileName || !findFileSize {
		return nil, errors.Errorf("SQL 语句 %q 执行结果解析失败", query)
	}

	var binlogFiles []*entity.BinlogFile

	for _, row := range rows {
		name, nameOk := row["Log_name"].(string)
		size, sizeOk := row["File_size"].(uint64)
		if !nameOk || !sizeOk {
			return nil, errors.Errorf("SQL 语句 %q 执行结果解析失败", query)
		}
		_, seq, err := ParseBinlogName(name)
		if err != nil {
			return nil, errors.Wrapf(err, "SQL 语句 %q 执行结果解析失败", query)
		}
		binlogFile := &entity.BinlogFile{
			Name:     name,
			Size:     int64(size),
			Sequence: seq,
		}
		binlogFiles = append(binlogFiles, binlogFile)
	}

	return sortBinlogFiles(binlogFiles), nil
}

var regexpBinlogInfo = regexp.MustCompile("CHANGE MASTER TO MASTER_LOG_FILE='([^.]+).([0-9]+)', MASTER_LOG_POS=([0-9]+);")

func readBinlogInfoFromBackup(reader io.Reader) (*entity.BinlogInfo, error) {
	matching := false
	r := bufio.NewReader(reader)
	const maxMatchRow = 100
	for i := 0; i < maxMatchRow; i++ {
		row, err := r.ReadString('\n')
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if !matching {
			if row == "-- Position to start replication or point-in-time recovery from\n" {
				matching = true
			} else {
				continue
			}
		}
		res := regexpBinlogInfo.FindStringSubmatch(row)
		if res == nil {
			continue
		}
		seq, err := strconv.ParseInt(res[2], 10, 64)
		if err != nil {
			return nil, err
		}
		pos, err := strconv.ParseInt(res[3], 10, 64)
		if err != nil {
			return nil, err
		}

		return &entity.BinlogInfo{
			FileName: fmt.Sprintf("%s.%s", res[1], res[2]),
			Sequence: seq,
			Position: pos,
		}, nil
	}
	return nil, errors.New("备份文件中未找到 binlog 信息")
}

// Use command like mysqlbinlog --start-datetime=targetTs binlog.000001 to parse the first binlog event position with timestamp equal or after targetTs.
func (svc *DbProgramMysql) GetBinlogEventPositionAtOrAfterTime(ctx context.Context, binlogName string, targetTime time.Time) (position int64, parseErr error) {
	binlogPath := svc.GetBinlogFilePath(binlogName)
	args := []string{
		// Local binlog file path.
		binlogPath,
		// Verify checksum binlog events.
		"--verify-binlog-checksum",
		// Tell mysqlbinlog to suppress the BINLOG statements for row events, which reduces the unneeded output.
		"--base64-output=DECODE-ROWS",
		// Instruct mysqlbinlog to start output only after encountering the first binlog event with timestamp equal or after targetTime.
		"--start-datetime", targetTime.Local().Format(time.DateTime),
	}
	cmd := exec.CommandContext(ctx, svc.getMysqlBin().MysqlbinlogPath, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	pr, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	defer func() {
		_ = cmd.Cancel()
		if err := cmd.Wait(); err != nil && parseErr != nil && stderr.Len() > 0 {
			parseErr = errors.Wrap(errors.New(stderr.String()), parseErr.Error())
		}
	}()

	for s := bufio.NewScanner(pr); s.Scan(); {
		line := s.Text()
		posParsed, found, err := parseBinlogEventPosInLine(line)
		if err != nil {
			return 0, errors.Wrap(err, "binlog 文件解析失败")
		}
		// When invoking mysqlbinlog with --start-datetime, the first valid event will always be FORMAT_DESCRIPTION_EVENT which should be skipped.
		if found && posParsed != 4 {
			return posParsed, nil
		}
	}
	return 0, errors.Errorf("在 %s 之后没有 binlog 事件", targetTime.Format(time.DateTime))
}

// ReplayBinlog replays the binlog for `originDatabase` from `startBinlogInfo.Position` to `targetTs`, read binlog from `binlogDir`.
func (svc *DbProgramMysql) ReplayBinlog(ctx context.Context, originalDatabase, targetDatabase string, restoreInfo *RestoreInfo) (replayErr error) {
	const (
		// Variable lower_case_table_names related.

		// LetterCaseOnDiskLetterCaseCmp stores table and database names using the letter case specified in the CREATE TABLE or CREATE DATABASE statement.
		// Name comparisons are case-sensitive.
		LetterCaseOnDiskLetterCaseCmp = 0
		// LowerCaseOnDiskLowerCaseCmp stores table names in lowercase on disk and name comparisons are not case-sensitive.
		LowerCaseOnDiskLowerCaseCmp = 1
		// LetterCaseOnDiskLowerCaseCmp stores table and database names are stored on disk using the letter case specified in the CREATE TABLE or CREATE DATABASE statement, but MySQL converts them to lowercase on lookup.
		// Name comparisons are not case-sensitive.
		LetterCaseOnDiskLowerCaseCmp = 2
	)

	caseVariable := "lower_case_table_names"
	identifierCaseSensitive, err := svc.getServerVariable(ctx, caseVariable)
	if err != nil {
		return err
	}

	identifierCaseSensitiveValue, err := strconv.Atoi(identifierCaseSensitive)
	if err != nil {
		return err
	}

	var originalDBName string
	switch identifierCaseSensitiveValue {
	case LetterCaseOnDiskLetterCaseCmp:
		originalDBName = originalDatabase
	case LowerCaseOnDiskLowerCaseCmp:
		originalDBName = strings.ToLower(originalDatabase)
	case LetterCaseOnDiskLowerCaseCmp:
		originalDBName = strings.ToLower(originalDatabase)
	default:
		return errors.Errorf("参数 %s 的值 %s 不符合预期: [%d, %d, %d] ", caseVariable, identifierCaseSensitive, 0, 1, 2)
	}

	// Extract the SQL statements from the binlog and replay them to the pitrDatabase via the mysql client by pipe.
	mysqlbinlogArgs := []string{
		// Verify checksum binlog events.
		"--verify-binlog-checksum",
		// Disable binary logging.
		"--disable-log-bin",
		// Create rewrite rules for databases when playing back from logs written in row-based format, so that we can apply the binlog to PITR database instead of the original database.
		"--rewrite-db", fmt.Sprintf("%s->%s", originalDBName, targetDatabase),
		// List entries for just this database. It's applied after the --rewrite-db option, so we should provide the rewritten database, i.e., pitrDatabase.
		"--database", targetDatabase,
		// Decode binary log from first event with position equal to or greater than argument.
		"--start-position", fmt.Sprintf("%d", restoreInfo.StartPosition),
		// 	Stop decoding binary log at first event with position equal to or greater than argument.
		"--stop-position", fmt.Sprintf("%d", restoreInfo.TargetPosition),
	}

	mysqlbinlogArgs = append(mysqlbinlogArgs, restoreInfo.GetBinlogPaths(svc.getBinlogDir(svc.dbInfo().InstanceId))...)

	mysqlArgs := []string{
		"--host", svc.dbInfo().Host,
		"--port", strconv.Itoa(svc.dbInfo().Port),
		"--user", svc.dbInfo().Username,
	}

	if svc.dbInfo().Password != "" {
		// The --password parameter of mysql/mysqlbinlog does not support the "--password PASSWORD" format (split by space).
		// If provided like that, the program will hang.
		mysqlArgs = append(mysqlArgs, fmt.Sprintf("--password=%s", svc.dbInfo().Password))
	}

	mysqlbinlogCmd := exec.CommandContext(ctx, svc.getMysqlBin().MysqlbinlogPath, mysqlbinlogArgs...)
	mysqlCmd := exec.CommandContext(ctx, svc.getMysqlBin().MysqlPath, mysqlArgs...)
	logx.Debug("Start replay binlog commands.",
		logx.String("mysqlbinlog", mysqlbinlogCmd.String()),
		logx.String("mysql", mysqlCmd.String()))
	defer func() {
		if replayErr == nil {
			logx.Debug("Replayed binlog successfully.")
		}
	}()

	mysqlRead, err := mysqlbinlogCmd.StdoutPipe()
	if err != nil {
		return errors.Wrap(err, "创建 mysqlbinlog 输出管道失败")
	}
	defer func() {
		_ = mysqlRead.Close()
	}()

	var mysqlbinlogErr, mysqlErr strings.Builder
	mysqlbinlogCmd.Stderr = &mysqlbinlogErr
	mysqlCmd.Stderr = &mysqlErr
	mysqlCmd.Stdout = os.Stdout
	mysqlCmd.Stdin = mysqlRead

	if err := mysqlbinlogCmd.Start(); err != nil {
		return errors.Wrap(err, "启动 mysqlbinlog 程序失败")
	}
	defer func() {
		if err := mysqlbinlogCmd.Wait(); err != nil {
			if replayErr != nil {
				replayErr = errors.Wrap(replayErr, "运行 mysqlbinlog 程序失败")
			} else {
				replayErr = errors.Errorf("运行 mysqlbinlog 程序失败: %s", mysqlbinlogErr.String())
			}
		}
	}()
	if err := mysqlCmd.Start(); err != nil {
		return errors.Wrap(err, "启动 mysql 程序失败")
	}
	if err := mysqlCmd.Wait(); err != nil {
		return errors.Errorf("运行 mysql 程序失败: %s", mysqlErr.String())
	}

	return nil
}

func (svc *DbProgramMysql) getServerVariable(_ context.Context, varName string) (string, error) {
	query := fmt.Sprintf("SHOW VARIABLES LIKE '%s'", varName)
	_, rows, err := svc.dbConn.Query(query)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", sql.ErrNoRows
	}

	var varNameFound, value string
	varNameFound = rows[0]["Variable_name"].(string)
	if varName != varNameFound {
		return "", errors.Errorf("未找到数据库参数 %s", varName)
	}
	value = rows[0]["Value"].(string)
	return value, nil
}

// CheckBinlogEnabled checks whether binlog is enabled for the current instance.
func (svc *DbProgramMysql) CheckBinlogEnabled(ctx context.Context) error {
	value, err := svc.getServerVariable(ctx, "log_bin")
	if err != nil {
		return err
	}
	if strings.ToUpper(value) != "ON" {
		return errors.Errorf("数据库未启用 binlog")
	}
	return nil
}

// CheckBinlogRowFormat checks whether the binlog format is ROW.
func (svc *DbProgramMysql) CheckBinlogRowFormat(ctx context.Context) error {
	value, err := svc.getServerVariable(ctx, "binlog_format")
	if err != nil {
		return err
	}
	if strings.ToUpper(value) != "ROW" {
		return errors.Errorf("binlog 格式 %s 不是行模式", value)
	}
	return nil
}

func runCmd(cmd *exec.Cmd) error {
	var stderr strings.Builder
	cmd.Stdout = os.Stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := cmd.Wait(); err != nil {
		return errors.New(stderr.String())
	}
	return nil
}

func (svc *DbProgramMysql) execute(database string, sql string) error {
	args := []string{
		"--host", svc.dbInfo().Host,
		"--port", strconv.Itoa(svc.dbInfo().Port),
		"--user", svc.dbInfo().Username,
		"--password=" + svc.dbInfo().Password,
		"--execute", sql,
	}
	if len(database) > 0 {
		args = append(args, database)
	}

	cmd := exec.Command(svc.getMysqlBin().MysqlPath, args...)
	logx.Debug("execute sql using mysql binary: ", cmd.String())
	if err := runCmd(cmd); err != nil {
		logx.Errorf("运行 mysql 程序失败: %v", err)
		return errors.Wrap(err, "运行 mysql 程序失败")
	}
	return nil
}

// sortBinlogFiles will sort binlog files in ascending order by their numeric extension.
// For mysql binlog, after the serial number reaches 999999, the next serial number will not return to 000000, but 1000000,
// so we cannot directly use string to compare lexicographical order.
func sortBinlogFiles(binlogFiles []*entity.BinlogFile) []*entity.BinlogFile {
	var sorted []*entity.BinlogFile
	sorted = append(sorted, binlogFiles...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Sequence < sorted[j].Sequence
	})
	return sorted
}

func parseBinlogEventTimeInLine(line string) (eventTs time.Time, found bool, err error) {
	// The target line starts with string like "#220421 14:49:26 server id 1"
	if !strings.Contains(line, "server id") {
		return time.Time{}, false, nil
	}
	if strings.Contains(line, "end_log_pos 0") {
		// https://github.com/mysql/mysql-server/blob/8.0/client/mysqlbinlog.cc#L1209-L1212
		// Fake events with end_log_pos=0 could be generated and we need to ignore them.
		return time.Time{}, false, nil
	}
	fields := strings.Fields(line)
	// fields should starts with ["#220421", "14:49:26", "server", "id", "1", "end_log_pos", "34794"]
	if len(fields) < 7 ||
		(len(fields[0]) != 7 || fields[2] != "server" || fields[3] != "id" || fields[5] != "end_log_pos") {
		return time.Time{}, false, errors.Errorf("found unexpected mysqlbinlog output line %q when parsing binlog event timestamp", line)
	}
	datetime, err := time.ParseInLocation("060102 15:04:05", fmt.Sprintf("%s %s", fields[0][1:], fields[1]), time.Local)
	if err != nil {
		return time.Time{}, false, err
	}
	return datetime, true, nil
}

func parseBinlogEventPosInLine(line string) (pos int64, found bool, err error) {
	// The mysqlbinlog output will contains a line starting with "# at 35065", which is the binlog event's start position.
	if !strings.HasPrefix(line, "# at ") {
		return 0, false, nil
	}
	// This is the line containing the start position of the binlog event.
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return 0, false, errors.Errorf("unexpected mysqlbinlog output line %q when parsing binlog event start position", line)
	}
	pos, err = strconv.ParseInt(fields[2], 10, 0)
	if err != nil {
		return 0, false, err
	}
	return pos, true, nil
}

// ParseBinlogName parses the numeric extension and the binary log base name by using split the dot.
// Examples:
//   - ("binlog.000001") => ("binlog", 1)
//   - ("binlog000001") => ("", err)
func ParseBinlogName(name string) (string, int64, error) {
	s := strings.Split(name, ".")
	if len(s) != 2 {
		return "", 0, errors.Errorf("failed to parse binlog extension, expecting two parts in the binlog file name %q but got %d", name, len(s))
	}
	seq, err := strconv.ParseInt(s[1], 10, 0)
	if err != nil {
		return "", 0, errors.Wrapf(err, "failed to parse the sequence number %s", s[1])
	}
	return s[0], seq, nil
}

// getBinlogDir gets the binlogDir.
func (svc *DbProgramMysql) getBinlogDir(instanceId uint64) string {
	return filepath.Join(
		svc.getBackupPath(),
		fmt.Sprintf("instance-%d", instanceId),
		"binlog")
}

func (svc *DbProgramMysql) getDbInstanceBackupRoot(instanceId uint64) string {
	return filepath.Join(
		svc.getBackupPath(),
		fmt.Sprintf("instance-%d", instanceId))
}

func (svc *DbProgramMysql) getDbBackupDir(instanceId, backupId uint64) string {
	return filepath.Join(
		svc.getBackupPath(),
		fmt.Sprintf("instance-%d", instanceId),
		fmt.Sprintf("backup-%d", backupId))
}
