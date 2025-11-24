package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// PrizeLevel represents prize level configuration
type PrizeLevel struct {
	Level string
	Score int
}

// PrizeCode represents a prize code with its level
type PrizeCode struct {
	Code  string
	Level string
	Used  bool
}

// splitByMulti splits answer strings like "A;B,C D" into ["A","B","C","D"]
// 分割多选答案
func splitByMulti(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return []string{}
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ';' || r == ',' || r == ' ' || r == '；' || r == '，'
	})
	out := []string{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// LoadQuestionsFromExcel reads questions from Sheet1
// expected header: id,type,question,options,answer,score
// 加载题目的excel
func LoadQuestionsFromExcel(path string) ([]Question, error) {
	f, err := excelize.OpenFile(path)
	if err != nil {
		return nil, err
	}
	// Step 1: Read quantity configuration from Sheet1
	typeQuantities := make(map[string]int)
	quantityRows, err := f.GetRows("Sheet1")
	if err != nil {
		return nil, err
	}

	for i, r := range quantityRows {
		if i == 0 {
			continue // skip header
		}
		if len(r) >= 3 {
			questionType := strings.TrimSpace(r[0])
			quantityStr := strings.TrimSpace(r[2])
			if quantity, err := strconv.Atoi(quantityStr); err == nil {
				typeQuantities[questionType] = quantity
			}
		}
	}

	// Step 2: Read all questions from Sheet2
	questionRows, err := f.GetRows("Sheet2")
	if err != nil {
		return nil, err
	}

	// Load questions and filter by quantity
	out := []Question{}
	typeCounts := make(map[string]int)

	for i, r := range questionRows {
		if i == 0 {
			continue // skip header
		}
		if len(r) < 4 {
			continue
		}

		id := strings.TrimSpace(r[0])
		typ := strings.TrimSpace(strings.ToLower(r[1]))
		question := r[2]

		// Check if we've reached the limit for this question type
		if maxQty, exists := typeQuantities[typ]; exists {
			if typeCounts[typ] >= maxQty {
				continue // Skip this question, we have enough of this type
			}
		} else {
			continue // Skip unknown question types
		}

		optsRaw := r[3]
		opts := []string{}
		for _, s := range strings.Split(optsRaw, ";") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			parts := strings.SplitN(s, ":", 2)
			if len(parts) == 2 {
				opts = append(opts, strings.TrimSpace(parts[1]))
			} else {
				opts = append(opts, s)
			}
		}

		ansSlice := []int{}
		if len(r) >= 5 && strings.TrimSpace(r[4]) != "" {
			parts := splitByMulti(r[4])
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				if len(p) == 1 && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) {
					idx := int(strings.ToUpper(p)[0] - 'A')
					ansSlice = append(ansSlice, idx)
				} else {
					if n, err := strconv.Atoi(p); err == nil {
						ansSlice = append(ansSlice, n)
					}
				}
			}
		}

		score := 1
		if len(r) >= 6 && strings.TrimSpace(r[5]) != "" {
			if v, err := strconv.Atoi(strings.TrimSpace(r[5])); err == nil {
				score = v
			}
		}

		out = append(out, Question{
			ID:      id,
			Type:    typ,
			Prompt:  question,
			Options: opts,
			Answer:  ansSlice,
			Score:   score,
		})

		typeCounts[typ]++
	}

	return out, nil

}

// LoadCodesFromExcel returns []string of codes (Sheet1, first column)
// 加载兑换码
func LoadCodesFromExcel(path string) ([]PrizeLevel, []PrizeCode, error) {
	f, err := excelize.OpenFile(path)
	if err != nil {
		return nil, nil, err
	}

	// Step 1: Read prize levels from Sheet1
	levelRows, err := f.GetRows("Sheet1")
	if err != nil {
		return nil, nil, err
	}

	prizeLevels := []PrizeLevel{}
	for i, r := range levelRows {
		if i == 0 {
			continue // skip header
		}
		if len(r) >= 2 {
			level := strings.TrimSpace(r[0])
			scoreStr := strings.TrimSpace(r[1])

			if level != "" && scoreStr != "" {
				if score, err := strconv.Atoi(scoreStr); err == nil {
					prizeLevels = append(prizeLevels, PrizeLevel{
						Level: level,
						Score: score,
					})
				}
			}
		}
	}

	// Step 2: Read prize codes from Sheet2
	codeRows, err := f.GetRows("Sheet2")
	if err != nil {
		return nil, nil, err
	}

	prizeCodes := []PrizeCode{}
	for i, r := range codeRows {
		if i == 0 {
			continue // skip header
		}
		if len(r) >= 2 {
			level := strings.TrimSpace(r[0])
			code := strings.TrimSpace(r[1])

			if level != "" && code != "" {
				prizeCodes = append(prizeCodes, PrizeCode{
					Code:  code,
					Level: level,
					Used:  false,
				})
			}
		}
	}

	return prizeLevels, prizeCodes, nil
}

// LoadResultPathFromExcel reads second row, first column as path
// 加载结果的
func LoadResultPathFromExcel(path string) (string, error) {
	f, err := excelize.OpenFile(path)
	if err != nil {
		return "", err
	}
	rows, err := f.GetRows("Sheet1")
	if err != nil {
		return "", err
	}
	if len(rows) < 2 || len(rows[1]) == 0 {
		return "", fmt.Errorf("result path excel 格式错误，第二行第一列应为保存路径")
	}
	return strings.TrimSpace(rows[1][0]), nil

}

// IsCodeUsedToday checks if a code has been used today
func IsCodeUsedToday(path, code string) (bool, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	}
	f, err := excelize.OpenFile(path)
	if err != nil {
		return false, err
	}

	rows, err := f.GetRows("Sheet1")
	if err != nil {
		return false, err
	}

	today := time.Now().Format("2006-01-02")

	for i, r := range rows {
		if i == 0 {
			continue // skip header
		}
		if len(r) >= 7 {
			recordCode := strings.TrimSpace(r[6])
			timestamp := strings.TrimSpace(r[0])

			if recordCode == code && strings.Contains(timestamp, today) {
				return true, nil
			}
		}
	}

	return false, nil
}

// SaveResultToExcel append a row to path (create file if not exist)
// 存储结果到excel中
func SaveResultToExcel(path, name, phone, idCard, maskName, maskPhone, maskIdCard string, score, total int, code string, detail interface{}) error {
	dir := filepath.Dir(path)
	if dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}
	var f *excelize.File
	newFile := false
	if _, err := os.Stat(path); os.IsNotExist(err) {
		newFile = true
		f = excelize.NewFile()
	} else {
		var err error
		f, err = excelize.OpenFile(path)
		if err != nil {
			return err
		}
	}
	sheet := "Sheet1"
	if newFile {
		_ = f.SetSheetRow(sheet, "A1", &[]interface{}{"timestamp", "name", "phone", "idCard", "maskName", "maskPhone", "maskIdCard", "score", "total", "code", "detail"})
	}
	rows, _ := f.GetRows(sheet)
	rowIdx := len(rows) + 1
	detailB, _ := json.Marshal(detail)
	row := []interface{}{time.Now().Format(time.RFC3339), name, phone, idCard, maskName, maskPhone, maskIdCard, score, total, code, string(detailB)}
	cell, _ := excelize.CoordinatesToCellName(1, rowIdx)
	if err := f.SetSheetRow(sheet, cell, &row); err != nil {
		return err
	}
	if err := f.SaveAs(path); err != nil {
		return err
	}
	return nil
}
