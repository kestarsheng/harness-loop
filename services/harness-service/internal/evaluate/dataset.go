package evaluate

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// EvalDataset is a persistent test set used as ground truth for evaluation.
// It holds a collection of DatasetCases that represent the "exam questions" for
// scoring prompt quality.
type EvalDataset struct {
	ID          string `gorm:"primaryKey"`
	Name        string `gorm:"uniqueIndex:idx_dataset_name_tenant"`
	Description string
	TenantID    string `gorm:"uniqueIndex:idx_dataset_name_tenant"`
	CaseCount   int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// DatasetCase is a single test case within a dataset.
type DatasetCase struct {
	ID        string `gorm:"primaryKey"`
	DatasetID string `gorm:"index"`
	Input     string // User input / question
	Expected  string // Expected output (used for automatic scoring)
	Context   string // Optional RAG context or background info
	Tags      string // JSON array for categorization (e.g. ["jailbreak","normal","edge_case"])
	CreatedAt time.Time
}

// DatasetEngine manages evaluation datasets and cases.
type DatasetEngine struct {
	db *gorm.DB
}

// NewDatasetEngine creates a new dataset engine.
func NewDatasetEngine(db *gorm.DB) *DatasetEngine {
	return &DatasetEngine{db: db}
}

// AutoMigrate creates database tables for dataset models.
func (e *DatasetEngine) AutoMigrate() error {
	if e.db == nil {
		return nil
	}
	return e.db.AutoMigrate(&EvalDataset{}, &DatasetCase{})
}

// CreateDataset creates a new evaluation dataset.
func (e *DatasetEngine) CreateDataset(ctx context.Context, ds *EvalDataset) error {
	if ds.ID == "" {
		ds.ID = uuid.New().String()
	}
	ds.CreatedAt = time.Now()
	ds.UpdatedAt = time.Now()

	if e.db != nil {
		if err := e.db.Create(ds).Error; err != nil {
			return fmt.Errorf("create dataset: %w", err)
		}
	}
	return nil
}

// GetDataset retrieves a dataset by ID.
func (e *DatasetEngine) GetDataset(ctx context.Context, id string) (*EvalDataset, error) {
	if e.db != nil {
		var ds EvalDataset
		if err := e.db.First(&ds, "id = ?", id).Error; err != nil {
			return nil, fmt.Errorf("dataset not found: %w", err)
		}
		return &ds, nil
	}
	return nil, fmt.Errorf("dataset not found: %s", id)
}

// ListDatasets lists datasets for a tenant.
func (e *DatasetEngine) ListDatasets(ctx context.Context, tenantID string) ([]*EvalDataset, error) {
	if e.db != nil {
		var datasets []*EvalDataset
		query := e.db.Model(&EvalDataset{})
		if tenantID != "" {
			query = query.Where("tenant_id = ?", tenantID)
		}
		if err := query.Order("created_at DESC").Find(&datasets).Error; err != nil {
			return nil, fmt.Errorf("list datasets: %w", err)
		}
		return datasets, nil
	}
	return nil, nil
}

// DeleteDataset deletes a dataset and all its cases.
func (e *DatasetEngine) DeleteDataset(ctx context.Context, id string) error {
	if e.db != nil {
		if err := e.db.Where("dataset_id = ?", id).Delete(&DatasetCase{}).Error; err != nil {
			return fmt.Errorf("delete cases: %w", err)
		}
		if err := e.db.Delete(&EvalDataset{}, "id = ?", id).Error; err != nil {
			return fmt.Errorf("delete dataset: %w", err)
		}
	}
	return nil
}

// AddCase adds a single test case to a dataset.
func (e *DatasetEngine) AddCase(ctx context.Context, c *DatasetCase) error {
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	c.CreatedAt = time.Now()

	if e.db != nil {
		if err := e.db.Create(c).Error; err != nil {
			return fmt.Errorf("add case: %w", err)
		}
		// Update case count
		e.db.Model(&EvalDataset{}).Where("id = ?", c.DatasetID).
			UpdateColumn("case_count", gorm.Expr("case_count + 1"))
	}
	return nil
}

// AddCases adds multiple test cases to a dataset in a batch.
func (e *DatasetEngine) AddCases(ctx context.Context, cases []*DatasetCase) error {
	if len(cases) == 0 {
		return nil
	}

	for _, c := range cases {
		if c.ID == "" {
			c.ID = uuid.New().String()
		}
		c.CreatedAt = time.Now()
	}

	if e.db != nil {
		if err := e.db.CreateInBatches(cases, 100).Error; err != nil {
			return fmt.Errorf("add cases batch: %w", err)
		}
		// Update case count
		datasetID := cases[0].DatasetID
		e.db.Model(&EvalDataset{}).Where("id = ?", datasetID).
			UpdateColumn("case_count", gorm.Expr("case_count + ?", len(cases)))
	}
	return nil
}

// ListCases lists all cases in a dataset.
func (e *DatasetEngine) ListCases(ctx context.Context, datasetID string) ([]*DatasetCase, error) {
	if e.db != nil {
		var cases []*DatasetCase
		if err := e.db.Where("dataset_id = ?", datasetID).Order("created_at ASC").Find(&cases).Error; err != nil {
			return nil, fmt.Errorf("list cases: %w", err)
		}
		return cases, nil
	}
	return nil, nil
}

// DeleteCase deletes a single test case.
func (e *DatasetEngine) DeleteCase(ctx context.Context, id string) error {
	if e.db != nil {
		// Get dataset ID first for count update
		var c DatasetCase
		if err := e.db.First(&c, "id = ?", id).Error; err != nil {
			return fmt.Errorf("case not found: %w", err)
		}
		if err := e.db.Delete(&DatasetCase{}, "id = ?", id).Error; err != nil {
			return fmt.Errorf("delete case: %w", err)
		}
		e.db.Model(&EvalDataset{}).Where("id = ?", c.DatasetID).
			UpdateColumn("case_count", gorm.Expr("GREATEST(case_count - 1, 0)"))
	}
	return nil
}

// ImportJSON imports cases from a JSON array.
// Expected format: [{"input": "...", "expected": "...", "context": "...", "tags": ["..."]}]
func (e *DatasetEngine) ImportJSON(ctx context.Context, datasetID string, jsonData []byte) (int, error) {
	var rawCases []struct {
		Input    string   `json:"input"`
		Expected string   `json:"expected"`
		Context  string   `json:"context"`
		Tags     []string `json:"tags"`
	}
	if err := json.Unmarshal(jsonData, &rawCases); err != nil {
		return 0, fmt.Errorf("parse JSON: %w", err)
	}

	cases := make([]*DatasetCase, 0, len(rawCases))
	for _, rc := range rawCases {
		tagsJSON, _ := json.Marshal(rc.Tags)
		cases = append(cases, &DatasetCase{
			DatasetID: datasetID,
			Input:     rc.Input,
			Expected:  rc.Expected,
			Context:   rc.Context,
			Tags:      string(tagsJSON),
		})
	}

	if err := e.AddCases(ctx, cases); err != nil {
		return 0, err
	}
	return len(cases), nil
}

// ImportCSV imports cases from CSV data.
// Expected columns: input, expected, context, tags (comma-separated)
func (e *DatasetEngine) ImportCSV(ctx context.Context, datasetID string, csvData io.Reader) (int, error) {
	reader := csv.NewReader(csvData)

	// Read header
	header, err := reader.Read()
	if err != nil {
		return 0, fmt.Errorf("read CSV header: %w", err)
	}

	// Map column indices
	colIndex := make(map[string]int)
	for i, h := range header {
		colIndex[strings.TrimSpace(strings.ToLower(h))] = i
	}

	inputCol, hasInput := colIndex["input"]
	expectedCol, hasExpected := colIndex["expected"]
	if !hasInput || !hasExpected {
		return 0, fmt.Errorf("CSV must have 'input' and 'expected' columns")
	}

	contextCol := colIndex["context"]
	tagsCol := colIndex["tags"]

	var cases []*DatasetCase
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read CSV row: %w", err)
		}

		input := ""
		if inputCol < len(row) {
			input = row[inputCol]
		}
		expected := ""
		if expectedCol < len(row) {
			expected = row[expectedCol]
		}
		context := ""
		if contextCol < len(row) {
			context = row[contextCol]
		}
		tags := "[]"
		if tagsCol < len(row) && row[tagsCol] != "" {
			tagList := strings.Split(row[tagsCol], ",")
			for i := range tagList {
				tagList[i] = strings.TrimSpace(tagList[i])
			}
			tagsJSON, _ := json.Marshal(tagList)
			tags = string(tagsJSON)
		}

		cases = append(cases, &DatasetCase{
			DatasetID: datasetID,
			Input:     input,
			Expected:  expected,
			Context:   context,
			Tags:      tags,
		})
	}

	if len(cases) == 0 {
		return 0, nil
	}

	if err := e.AddCases(ctx, cases); err != nil {
		return 0, err
	}
	return len(cases), nil
}
