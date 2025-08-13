package excel

import (
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/360EntSecGroup-Skylar/excelize/v2"
	"github.com/east-eden/server/define"
	"github.com/east-eden/server/utils"
	"github.com/emirpasic/gods/maps/treemap"
	map_utils "github.com/emirpasic/gods/utils"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	"github.com/spf13/cast"
	"github.com/thanhpk/randstr"
)

var (
	RowOffset int = 2 // 第一行数据偏移
	ColOffset int = 2 // 第一列数据偏移
)

var (
	entryLoaders       sync.Map                 // all entry loaders
	entryManualLoaders sync.Map                 // all entry manual loaders
	excelFileRaws      map[string]*ExcelFileRaw // all excel file raw data
)

type ExcelRowData map[string]any

// Entry should implement Load function
type EntryLoader interface {
	Load(*ExcelFileRaw) error
}

// Entry should implement ManualLoad
type EntryManualLoader interface {
	ManualLoad(*ExcelFileRaw) error
}

// Excel field raw data
type ExcelFieldRaw struct {
	name string
	tp   string
	desc string
	tag  string
	idx  int  // field index in excel file
	imp  bool // need import
}

// Excel file raw data
type ExcelFileRaw struct {
	Filename string
	Keys     []string
	HasMap   bool
	FieldRaw *treemap.Map
	CellData []ExcelRowData
}

func init() {
	excelFileRaws = make(map[string]*ExcelFileRaw)
}

func AddEntryLoader(name string, e EntryLoader) {
	entryLoaders.Store(name, e)
}

func AddEntryManualLoader(name string, e EntryManualLoader) {
	entryManualLoaders.Store(name, e)
}

func parseRows(dirPath, filename string, rows [][]string) (*ExcelFileRaw, error) {

	fileRaw := &ExcelFileRaw{
		Filename: filename,
		FieldRaw: treemap.NewWithStringComparator(),
		CellData: make([]ExcelRowData, 0),
	}

	// rotate config excel files
	if strings.Contains(fileRaw.Filename, "Config") {
		newRows := make([][]string, len(rows[RowOffset]))
		for n := 0; n < len(newRows); n++ {
			newRows[n] = make([]string, len(rows))
		}

		for n := 0; n < len(rows); n++ {
			for m := 0; m < len(rows[RowOffset]); m++ {
				newRows[m][n] = rows[n][m]
			}
		}
		parseExcelData(newRows, fileRaw)
	} else {
		parseExcelData(rows, fileRaw)
	}

	return fileRaw, nil
}

func getAllExcelFileNames(readExcelPath string) []string {
	dir, err := os.ReadDir(readExcelPath)
	if !utils.ErrCheck(err, "read dir failed", readExcelPath) {
		return []string{}
	}

	// escape dir and ~$***.xlsx
	fileNames := make([]string, 0, len(dir))
	for _, fi := range dir {
		if !fi.IsDir() && strings.HasSuffix(fi.Name(), ".xlsx") && !strings.HasPrefix(fi.Name(), "~$") {
			fileNames = append(fileNames, fi.Name())
		}
	}

	return fileNames
}

func getAllCsvFileNames(readExcelPath string) []string {
	dir, err := os.ReadDir(readExcelPath)
	if !utils.ErrCheck(err, "read dir failed", readExcelPath) {
		return []string{}
	}

	// escape dir and ~$***.csv
	fileNames := make([]string, 0, len(dir))
	for _, fi := range dir {
		if !fi.IsDir() && strings.HasSuffix(fi.Name(), ".csv") && !strings.HasPrefix(fi.Name(), "~$") {
			fileNames = append(fileNames, fi.Name())
		}
	}

	return fileNames
}

// load all excel files and translate to CSV
func loadExcelToCSV(dirPath string, exportCsvPath string, fileNames []string) {
	wg := utils.WaitGroupWrapper{}
	mu := sync.Mutex{}
	for _, v := range fileNames {
		name := v
		wg.Wrap(func() {
			defer utils.CaptureException(name)

			filePath := fmt.Sprintf("%s%s", dirPath, name)
			xlsxFile, err := excelize.OpenFile(filePath)
			if !utils.ErrCheck(err, "excelize.OpenFile failed", filePath) {
				return
			}

			// read rows from excel files
			rows, err := xlsxFile.GetRows(xlsxFile.GetSheetName(0))
			if !utils.ErrCheck(err, "xlsxFile.GetRows failed", filePath) {
				return
			}

			// parse rows to *ExcelFileRaw
			csvName := strings.Replace(name, ".xlsx", ".csv", -1)
			rowDatas, err := parseRows(dirPath, csvName, rows)
			if !utils.ErrCheck(err, "parseRows failed", name) {
				return
			}

			// write rows to csv files
			fiPath := fmt.Sprintf("%s%s", exportCsvPath, csvName)
			fi, err := os.OpenFile(fiPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
			if !utils.ErrCheck(err, "OpenFile failed", fiPath) {
				return
			}

			w := csv.NewWriter(fi)
			err = w.WriteAll(rows)
			if !utils.ErrCheck(err, "csv.WriteAll failed", fiPath) {
				return
			}

			mu.Lock()
			excelFileRaws[csvName] = rowDatas
			mu.Unlock()
		})
	}
	wg.Wait()
}

func readCSV(dirPath string, fileNames []string) {
	wg := utils.WaitGroupWrapper{}
	mu := sync.Mutex{}
	for _, v := range fileNames {
		name := v
		wg.Wrap(func() {
			defer utils.CaptureException(name)
			fiPath := fmt.Sprintf("%s%s", dirPath, name)
			fi, err := os.OpenFile(fiPath, os.O_RDONLY, 0666)
			if !utils.ErrCheck(err, "OpenFile failed", fiPath) {
				return
			}

			r := csv.NewReader(fi)
			r.FieldsPerRecord = -1
			rows, err := r.ReadAll()
			if !utils.ErrCheck(err, "ReadAll failed", fiPath) {
				return
			}

			rowDatas, err := parseRows(dirPath, name, rows)
			if !utils.ErrCheck(err, "parseRows failed", name) {
				return
			}

			mu.Lock()
			excelFileRaws[name] = rowDatas
			mu.Unlock()
		})
	}
	wg.Wait()
}

// generate go code from excel file
func generateAllCodes(exportPath string, fileNames []string) {
	wg := utils.WaitGroupWrapper{}
	for _, v := range fileNames {
		name := strings.Replace(v, ".xlsx", ".csv", -1)
		wg.Wrap(func() {
			defer utils.CaptureException(name)
			err := generateCode(exportPath, excelFileRaws[name])
			if !utils.ErrCheck(err, "generateCode failed", exportPath, name) {
				return
			}

			log.Info().Str("file_name", name).Str("export_dir", exportPath).Caller().Msg("generate go code success")
		})
	}

	wg.Wait()
}

func Generate(readExcelPath, exportGoPath, exportCsvPath string) {
	fileNames := getAllExcelFileNames(readExcelPath)
	loadExcelToCSV(readExcelPath, exportCsvPath, fileNames)
	generateAllCodes(exportGoPath, fileNames)
}

// ExcelReader handles the reading and loading of excel entries
type ExcelReader struct {
	dirPath   string
	fileNames []string
	stats     *LoadingStats
}

// LoadingStats tracks the loading statistics
type LoadingStats struct {
	TotalFiles     int
	LoadedFiles    int
	FailedFiles    int
	TotalLoaders   int
	SuccessLoaders int
	FailedLoaders  int
	StartTime      time.Time
	EndTime        time.Time
}

// NewExcelReader creates a new ExcelReader instance
func NewExcelReader(dirPath string) *ExcelReader {
	return &ExcelReader{
		dirPath: dirPath,
		stats: &LoadingStats{
			StartTime: time.Now(),
		},
	}
}

// ReadAllEntries reads and loads all excel entries with improved error handling and logging
func ReadAllEntries(dirPath string) {
	reader := NewExcelReader(dirPath)
	reader.Execute()
}

// Execute performs the complete excel reading and loading process
func (r *ExcelReader) Execute() {
	log.Info().Str("dir_path", r.dirPath).Msg("starting excel entries reading process")

	// Step 1: Discover CSV files
	if err := r.discoverFiles(); err != nil {
		log.Error().Err(err).Msg("failed to discover CSV files")
		return
	}

	// Step 2: Read CSV files
	if err := r.readCSVFiles(); err != nil {
		log.Error().Err(err).Msg("failed to read CSV files")
		return
	}

	// Step 3: Load entries using registered loaders
	if err := r.loadEntries(); err != nil {
		log.Error().Err(err).Msg("failed to load entries")
		return
	}

	// Step 4: Load entries using manual loaders
	if err := r.loadManualEntries(); err != nil {
		log.Error().Err(err).Msg("failed to load manual entries")
		return
	}

	r.stats.EndTime = time.Now()
	r.logCompletionStats()
}

// discoverFiles discovers all CSV files in the directory
func (r *ExcelReader) discoverFiles() error {
	r.fileNames = getAllCsvFileNames(r.dirPath)
	r.stats.TotalFiles = len(r.fileNames)

	if r.stats.TotalFiles == 0 {
		return fmt.Errorf("no CSV files found in directory: %s", r.dirPath)
	}

	log.Info().Int("file_count", r.stats.TotalFiles).Msg("discovered CSV files")
	return nil
}

// readCSVFiles reads all discovered CSV files
func (r *ExcelReader) readCSVFiles() error {
	log.Info().Msg("reading CSV files...")

	readCSV(r.dirPath, r.fileNames)
	r.stats.LoadedFiles = len(excelFileRaws)

	log.Info().
		Int("loaded_files", r.stats.LoadedFiles).
		Int("total_files", r.stats.TotalFiles).
		Msg("CSV files reading completed")

	return nil
}

// loadEntries loads entries using registered entry loaders
func (r *ExcelReader) loadEntries() error {
	log.Info().Msg("loading entries with registered loaders...")

	wg := utils.WaitGroupWrapper{}
	var mu sync.Mutex

	entryLoaders.Range(func(k, v any) bool {
		entryName := k.(string)
		loader := v.(EntryLoader)
		r.stats.TotalLoaders++

		wg.Wrap(func() {
			defer utils.CaptureException(entryName)

			if err := r.loadSingleEntry(entryName, loader); err != nil {
				mu.Lock()
				r.stats.FailedLoaders++
				mu.Unlock()
				log.Error().Err(err).Str("entry_name", entryName).Msg("entry loader failed")
			} else {
				mu.Lock()
				r.stats.SuccessLoaders++
				mu.Unlock()
				log.Debug().Str("entry_name", entryName).Msg("entry loader succeeded")
			}
		})

		return true
	})
	wg.Wait()

	log.Info().
		Int("success_loaders", r.stats.SuccessLoaders).
		Int("failed_loaders", r.stats.FailedLoaders).
		Msg("entry loaders completed")

	return nil
}

// loadManualEntries loads entries using manual entry loaders
func (r *ExcelReader) loadManualEntries() error {
	log.Info().Msg("loading entries with manual loaders...")

	wg := utils.WaitGroupWrapper{}
	var mu sync.Mutex
	manualLoaderCount := 0
	successManualLoaders := 0
	failedManualLoaders := 0

	entryManualLoaders.Range(func(k, v any) bool {
		entryName := k.(string)
		loader := v.(EntryManualLoader)
		manualLoaderCount++

		wg.Wrap(func() {
			defer utils.CaptureException(entryName)

			if err := r.loadSingleManualEntry(entryName, loader); err != nil {
				mu.Lock()
				failedManualLoaders++
				mu.Unlock()
				log.Error().Err(err).Str("entry_name", entryName).Msg("manual entry loader failed")
			} else {
				mu.Lock()
				successManualLoaders++
				mu.Unlock()
				log.Debug().Str("entry_name", entryName).Msg("manual entry loader succeeded")
			}
		})

		return true
	})
	wg.Wait()

	log.Info().
		Int("success_manual_loaders", successManualLoaders).
		Int("failed_manual_loaders", failedManualLoaders).
		Int("total_manual_loaders", manualLoaderCount).
		Msg("manual entry loaders completed")

	return nil
}

// loadSingleEntry loads a single entry using the provided loader
func (r *ExcelReader) loadSingleEntry(entryName string, loader EntryLoader) error {
	fileRaw, exists := excelFileRaws[entryName]
	if !exists {
		return fmt.Errorf("excel file raw data not found for entry: %s", entryName)
	}

	if fileRaw == nil {
		return fmt.Errorf("excel file raw data is nil for entry: %s", entryName)
	}

	return loader.Load(fileRaw)
}

// loadSingleManualEntry loads a single entry using the provided manual loader
func (r *ExcelReader) loadSingleManualEntry(entryName string, loader EntryManualLoader) error {
	fileRaw, exists := excelFileRaws[entryName]
	if !exists {
		return fmt.Errorf("excel file raw data not found for manual entry: %s", entryName)
	}

	if fileRaw == nil {
		return fmt.Errorf("excel file raw data is nil for manual entry: %s", entryName)
	}

	return loader.ManualLoad(fileRaw)
}

// logCompletionStats logs the completion statistics
func (r *ExcelReader) logCompletionStats() {
	duration := r.stats.EndTime.Sub(r.stats.StartTime)

	log.Info().
		Str("duration", duration.String()).
		Int("total_files", r.stats.TotalFiles).
		Int("loaded_files", r.stats.LoadedFiles).
		Int("total_loaders", r.stats.TotalLoaders).
		Int("success_loaders", r.stats.SuccessLoaders).
		Int("failed_loaders", r.stats.FailedLoaders).
		Msg("all excel entries reading completed!")
}

func parseExcelData(rows [][]string, fileRaw *ExcelFileRaw) {

	typeNames := make([]string, len(rows[2])-ColOffset)
	typeValues := make([]string, len(rows[2])-ColOffset)
	for n := 0; n < len(rows); n++ {
		if rows[n] == nil {
			break
		}

		// load type name
		if n == RowOffset {
			for m := ColOffset; m < len(rows[n]); m++ {
				fieldName := rows[n][m]
				raw := &ExcelFieldRaw{
					idx: m - ColOffset,
				}

				// 无字段名不导出，随机生成字段名字符串
				if len(fieldName) == 0 {
					raw.imp = false
					fieldName = fmt.Sprintf("V%s", randstr.String(16))
				}

				// 首字段
				if m == ColOffset {
					fieldName = "Id"
				}

				raw.name = strings.Title(fieldName)
				if _, found := fileRaw.FieldRaw.Get(raw.name); found {
					_ = utils.ErrCheck(errors.New("duplicate field name"), "parseExcelData failed", raw.name, fileRaw.Filename)
					continue
				}

				raw.tag = fmt.Sprintf("`json:\"%s,omitempty\"`", raw.name)
				fileRaw.FieldRaw.Put(raw.name, raw)
				typeNames[m-ColOffset] = raw.name
			}
		}

		// load type desc
		// if n == RowOffset+1 {

		// }

		// load type control
		if n == RowOffset+2 {
			var strBuilder strings.Builder
			for m := ColOffset; m < len(rows[n]); m++ {
				fieldName := typeNames[m-ColOffset]
				fieldValue := rows[n][m]
				value, ok := fileRaw.FieldRaw.Get(fieldName)
				if !ok {
					log.Fatal().
						Caller().
						Str("filename", fileRaw.Filename).
						Str("fieldname", fieldName).
						Int("row", n).
						Int("col", m).
						Msg("parse excel data failed")
				}

				// 第一个字段默认主键
				if m == ColOffset {
					fileRaw.Keys = append(fileRaw.Keys, value.(*ExcelFieldRaw).name)

					// 去除换行
					desc := strings.Replace(value.(*ExcelFieldRaw).desc, "\n", ",", -1)

					strBuilder.Reset()
					strBuilder.WriteString(desc)
					strBuilder.WriteString(" 主键")
					value.(*ExcelFieldRaw).imp = true
					value.(*ExcelFieldRaw).desc = strBuilder.String()
					continue
				}

				// 带K标识的也是主键
				if strings.Contains(fieldValue, "K") {
					fileRaw.Keys = append(fileRaw.Keys, value.(*ExcelFieldRaw).name)

					// 去除换行
					desc := strings.Replace(value.(*ExcelFieldRaw).desc, "\n", ",", -1)
					strBuilder.Reset()
					strBuilder.WriteString(desc)
					strBuilder.WriteString(" 多主键之一")
					value.(*ExcelFieldRaw).desc = strBuilder.String()
				} else {
					// 去除换行
					desc := strings.Replace(rows[n-1][m], "\n", ",", -1)
					value.(*ExcelFieldRaw).desc = desc
				}

				if strings.Contains(fieldValue, "C") {
					value.(*ExcelFieldRaw).imp = false
				} else {
					value.(*ExcelFieldRaw).imp = true
				}
			}
		}

		// load type value
		if n == RowOffset+3 {
			for m := ColOffset; m < len(rows[n]); m++ {
				fieldName := typeNames[m-ColOffset]
				fieldValue := rows[n][m]
				convertType := convertType(fieldValue)

				value, ok := fileRaw.FieldRaw.Get(fieldName)
				if !ok {
					log.Fatal().
						Caller().
						Str("filename", fileRaw.Filename).
						Str("fieldname", fieldName).
						Int("row", n).
						Int("col", m).
						Msg("parse excel data failed")
				}

				if convertType == "*treemap.Map" {
					fileRaw.HasMap = true
				}

				if len(convertType) == 0 {
					value.(*ExcelFieldRaw).imp = false
				}

				value.(*ExcelFieldRaw).tp = convertType
				typeValues[m-ColOffset] = fieldValue
			}
		}

		// 客户端导出字段
		if n == RowOffset+2 {
			continue
		}

		// there is no actual data before row:7
		if n < RowOffset+4 {
			continue
		}

		// empty data row
		if len(rows[n][2]) == 0 {
			continue
		}

		// resize row
		if len(rows[n]) < len(rows[RowOffset]) {
			rows[n] = append(rows[n], make([]string, len(rows[RowOffset])-len(rows[n]))...)
		}
		rows[n] = rows[n][:len(rows[RowOffset])]

		mapRowData := make(map[string]any)
		for m := ColOffset; m < len(rows[n]); m++ {
			cellColIdx := m - ColOffset
			cellValString := rows[n][m]

			// set value
			convertedVal := convertValue(typeValues[cellColIdx], cellValString)
			mapRowData[typeNames[cellColIdx]] = convertedVal
		}

		fileRaw.CellData = append(fileRaw.CellData, mapRowData)
	}
}

// be tolerant with type names
func convertType(strType string) string {
	switch strType {
	case "String", "STRING":
		return "string"

	case "[]String", "String[]", "[]STRING":
		return "[]string"

	case "Int32", "Int", "INT", "int":
		return "int32"

	case "Number", "NUMBER", "number":
		return "decimal.Decimal"

	case "Float32", "Float", "FLOAT", "float":
		return "float32"

	case "[]Int32", "[]Int", "[]INT", "[]int":
		return "[]int32"

	case "[]Number", "[]NUMBER", "Number[]", "NUMBER[]", "number[]", "[]number":
		return "[]decimal.Decimal"

	case "Bool", "BOOL":
		return "bool"

	default:
		if strings.HasPrefix(strType, "map") || strings.HasPrefix(strType, "Map") {
			return "*treemap.Map"
		}

		return strType
	}
}

func convertValue(strType, strVal string) any {
	var cellVal any
	convertType := convertType(strType)

	switch convertType {
	case "int32":
		if len(strVal) == 0 {
			cellVal = int32(0)
		} else {
			cellVal = cast.ToInt32(strVal)
		}

	case "decimal.Decimal":
		if len(strVal) == 0 || strVal == "0" {
			cellVal = decimal.NewFromInt32(0)
		} else {
			cellVal, _ = decimal.NewFromString(strVal)
			// floatVal := cast.ToFloat64(strVal)
			// floatVal *= define.PercentBase
			// floatVal = math.Round(floatVal)
			// cellVal = int32(floatVal)
		}

	case "number":
		if len(strVal) == 0 || strVal == "0" {
			cellVal = int32(0)
		} else {
			floatVal, err := strconv.ParseFloat(strVal, 32)
			utils.ErrPrint(err, "convert cell value to number failed", strVal)

			floatVal *= define.PercentBase
			floatVal = math.Round(floatVal)
			cellVal = int32(floatVal)
		}

	case "float32":
		if len(strVal) == 0 {
			cellVal = float32(0)
		} else {
			cellVal = cast.ToFloat32(strVal)
		}

	case "[]int32":
		cellVals := strings.Split(strVal, ",")
		arrVals := make([]any, len(cellVals))
		for k, v := range cellVals {
			arrVals[k] = convertValue("int32", v)
		}
		cellVal = arrVals

	case "[]decimal.Decimal":
		cellVals := strings.Split(strVal, ",")
		arrVals := make([]any, len(cellVals))
		for k, v := range cellVals {
			arrVals[k] = convertValue("number", v)
		}
		cellVal = arrVals

	case "[]float32":
		cellVals := strings.Split(strVal, ",")
		arrVals := make([]any, len(cellVals))
		for k, v := range cellVals {
			arrVals[k] = convertValue("float32", v)
		}
		cellVal = arrVals

	case "[]string":
		cellVals := strings.Split(strVal, ",")
		arrVals := make([]any, len(cellVals))
		for k, v := range cellVals {
			arrVals[k] = convertValue("string", v)
		}
		cellVal = arrVals

	case "*treemap.Map":
		cellVal = convertMapValue(strType, strVal)

	case "bool":
		cellVal = cast.ToBool(strVal)

	default:
		// default string value
		if len(strVal) == 0 {
			cellVal = ""
		} else {
			cellVal = strVal
		}
	}

	return cellVal
}

func convertMapValue(strType, strVal string) any {
	// split type and value, example: map[int32]string => "int32" and "string"
	ts := strings.Split(strType, "[")
	t := ts[len(ts)-1]
	tt := strings.Split(t, "]")
	keyType := convertType(tt[0])
	valueType := convertType(tt[1])

	m := treemap.NewWith(func() map_utils.Comparator {
		switch keyType {
		case "int32":
			return map_utils.Int32Comparator
		case "string":
			return map_utils.StringComparator
		case "float32":
			return map_utils.Float32Comparator
		default:
			return map_utils.Int32Comparator
		}
	}())

	mapValues := strings.Split(strVal, ",")
	for _, oneMapValue := range mapValues {
		fields := strings.Split(oneMapValue, ":")
		if len(fields) < 2 {
			continue
		}

		k := convertValue(keyType, fields[0])
		v := convertValue(valueType, fields[1])
		m.Put(k, v)
	}

	return m
}
