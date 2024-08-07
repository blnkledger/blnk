package blnk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jerry-enebeli/blnk/database"
	"github.com/jerry-enebeli/blnk/internal/notification"
	"github.com/jerry-enebeli/blnk/model"
	"github.com/texttheater/golang-levenshtein/levenshtein"
	"github.com/wacul/ptr"
)

const (
	StatusStarted    = "started"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
)

type transactionProcessor struct {
	reconciliation    model.Reconciliation
	progress          model.ReconciliationProgress
	reconciler        reconciler
	matches           []model.Match
	unmatched         []string
	datasource        database.IDataSource
	progressSaveCount int
}

type reconciler func(ctx context.Context, txns []*model.Transaction) ([]model.Match, []string)

func detectFileType(data []byte, filename string) (string, error) {
	// First, try to detect by file extension
	if ext := strings.ToLower(filepath.Ext(filename)); ext != "" {
		if mimeType := mime.TypeByExtension(ext); mimeType != "" {
			return mimeType, nil
		}
	}

	// If extension doesn't work, try content-based detection
	mimeType := http.DetectContentType(data)

	switch mimeType {
	case "application/octet-stream":
		// Further analysis for common types
		if looksLikeCSV(data) {
			return "text/csv", nil
		}
		if json.Valid(data) {
			return "application/json", nil
		}
	case "text/plain":
		// Additional checks for text-based formats
		if looksLikeCSV(data) {
			return "text/csv", nil
		}
		if json.Valid(data) {
			return "application/json", nil
		}
	case "text/csv; charset=utf-8":
		// Fix for CSV files with charset
		return "text/csv", nil
	}

	return mimeType, nil
}

func looksLikeCSV(data []byte) bool {
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) < 2 {
		return false
	}

	// Check if all lines have the same number of fields
	fields := bytes.Count(lines[0], []byte(",")) + 1
	for _, line := range lines[1:] {
		if len(line) == 0 {
			continue // Skip empty lines
		}
		if bytes.Count(line, []byte(","))+1 != fields {
			return false
		}
	}

	return fields > 1 // Require at least two fields to be considered CSV
}

func parseFloat(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0 // Return 0 if parsing fails
	}
	return f
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{} // Return zero time if parsing fails
	}
	return t
}

func contains(slice []string, item string) bool {
	for _, a := range slice {
		if a == item {
			return true
		}
	}
	return false
}

func (s *Blnk) parseAndStoreCSV(ctx context.Context, uploadID, source string, reader io.Reader) error {
	csvReader := csv.NewReader(bufio.NewReader(reader))

	// Read headers
	headers, err := csvReader.Read()
	if err != nil {
		return fmt.Errorf("error reading CSV headers: %w", err)
	}

	// Create a map of expected columns to their indices
	columnMap, err := createColumnMap(headers)
	if err != nil {
		return err
	}

	return s.processCSVRows(ctx, uploadID, source, csvReader, columnMap)
}

func (s *Blnk) processCSVRows(ctx context.Context, uploadID, source string, csvReader *csv.Reader, columnMap map[string]int) error {
	var errs []error
	rowNum := 1 // Start at 1 to account for header row

	for {
		record, err := csvReader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("error reading row %d: %w", rowNum, err))
			continue
		}

		rowNum++

		externalTxn, err := parseExternalTransaction(record, columnMap, source)
		if err != nil {
			errs = append(errs, fmt.Errorf("error parsing row %d: %w", rowNum, err))
			continue
		}

		if err := s.storeExternalTransaction(ctx, uploadID, externalTxn); err != nil {
			errs = append(errs, fmt.Errorf("error storing transaction from row %d: %w", rowNum, err))
		}

		// Periodically check context for cancellation
		if rowNum%1000 == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("encountered %d errors while processing CSV: %v", len(errs), errs)
	}

	return nil
}

func createColumnMap(headers []string) (map[string]int, error) {
	requiredColumns := []string{"ID", "Amount", "Date"}
	columnMap := make(map[string]int)

	for i, header := range headers {
		columnMap[strings.ToLower(strings.TrimSpace(header))] = i
	}

	for _, col := range requiredColumns {
		if _, exists := columnMap[strings.ToLower(col)]; !exists {
			return nil, fmt.Errorf("required column '%s' not found in CSV", col)
		}
	}

	return columnMap, nil
}

func parseExternalTransaction(record []string, columnMap map[string]int, source string) (model.ExternalTransaction, error) {
	if len(record) != len(columnMap) {
		return model.ExternalTransaction{}, fmt.Errorf("incorrect number of fields in record")
	}

	id, err := getRequiredField(record, columnMap, "id")
	if err != nil {
		return model.ExternalTransaction{}, err
	}

	amountStr, err := getRequiredField(record, columnMap, "amount")
	if err != nil {
		return model.ExternalTransaction{}, err
	}

	currency, err := getRequiredField(record, columnMap, "currency")
	if err != nil {
		return model.ExternalTransaction{}, err
	}

	amount := parseFloat(amountStr)
	if err != nil {
		return model.ExternalTransaction{}, fmt.Errorf("invalid amount: %w", err)
	}

	reference, err := getRequiredField(record, columnMap, "reference")
	if err != nil {
		return model.ExternalTransaction{}, err
	}

	description, err := getRequiredField(record, columnMap, "description")
	if err != nil {
		return model.ExternalTransaction{}, err
	}

	dateStr, err := getRequiredField(record, columnMap, "date")
	if err != nil {
		return model.ExternalTransaction{}, err
	}
	date := parseTime(dateStr)
	if err != nil {
		return model.ExternalTransaction{}, fmt.Errorf("invalid date: %w", err)
	}

	return model.ExternalTransaction{
		ID:          id,
		Amount:      amount,
		Currency:    currency,
		Reference:   reference,
		Description: description,
		Date:        date,
		Source:      source,
	}, nil
}

func getRequiredField(record []string, columnMap map[string]int, field string) (string, error) {
	if index, exists := columnMap[field]; exists && index < len(record) {
		value := strings.TrimSpace(record[index])
		if value == "" {
			return "", fmt.Errorf("required field '%s' is empty", field)
		}
		return value, nil
	}
	return "", fmt.Errorf("required field '%s' not found in record", field)
}

func (s *Blnk) parseAndStoreJSON(ctx context.Context, uploadID, source string, reader io.Reader) (int, error) {
	decoder := json.NewDecoder(reader)
	var transactions []model.ExternalTransaction
	if err := decoder.Decode(&transactions); err != nil {
		return 0, err
	}

	for _, txn := range transactions {
		txn.Source = source
		if err := s.storeExternalTransaction(ctx, uploadID, txn); err != nil {
			return 0, err
		}
	}

	return len(transactions), nil
}

func (s *Blnk) UploadExternalData(ctx context.Context, source string, reader io.Reader, filename string) (string, int, error) {
	uploadID := model.GenerateUUIDWithSuffix("upload")

	// Create a temporary file to store the upload
	tempFile, err := s.createTempFile(filename)
	if err != nil {
		return "", 0, fmt.Errorf("error creating temporary file: %w", err)
	}
	defer s.cleanupTempFile(tempFile)

	// Copy the data to the temporary file
	if _, err := io.Copy(tempFile, reader); err != nil {
		return "", 0, fmt.Errorf("error copying upload data: %w", err)
	}

	// Seek to the beginning of the file
	if _, err := tempFile.Seek(0, 0); err != nil {
		return "", 0, fmt.Errorf("error seeking temporary file: %w", err)
	}

	// Read the first 512 bytes for file type detection
	header := make([]byte, 512)
	if _, err := tempFile.Read(header); err != nil && err != io.EOF {
		return "", 0, fmt.Errorf("error reading file header: %w", err)
	}

	// Detect the file type
	fileType, err := detectFileType(header, filename)
	if err != nil {
		return "", 0, fmt.Errorf("error detecting file type: %w", err)
	}

	// Seek back to the beginning of the file
	if _, err := tempFile.Seek(0, 0); err != nil {
		return "", 0, fmt.Errorf("error seeking temporary file: %w", err)
	}

	total := 0

	// Parse and store the data based on the detected file type
	switch fileType {
	case "text/csv":
		err = s.parseAndStoreCSV(ctx, uploadID, source, tempFile)
	case "application/json":
		total, err = s.parseAndStoreJSON(ctx, uploadID, source, tempFile)
	case "text/csv; charset=utf-8":
		err = s.parseAndStoreCSV(ctx, uploadID, source, tempFile)
	default:
		return "", 0, fmt.Errorf("unsupported file type: %s", fileType)
	}

	if err != nil {
		return "", 0, err
	}

	return uploadID, total, nil
}

func (s *Blnk) createTempFile(originalFilename string) (*os.File, error) {
	// Create a temporary directory if it doesn't exist
	tempDir := filepath.Join(os.TempDir(), "blnk_uploads")
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return nil, fmt.Errorf("error creating temporary directory: %w", err)
	}

	// Create a temporary file with a prefix based on the original filename
	prefix := fmt.Sprintf("%s_", filepath.Base(originalFilename))
	tempFile, err := os.CreateTemp(tempDir, prefix)
	if err != nil {
		return nil, fmt.Errorf("error creating temporary file: %w", err)
	}

	return tempFile, nil
}

func (s *Blnk) cleanupTempFile(file *os.File) {
	if file != nil {
		filename := file.Name()
		file.Close()
		if err := os.Remove(filename); err != nil {
			log.Printf("Error removing temporary file %s: %v", filename, err)
		}
	}
}

func (s *Blnk) storeExternalTransaction(ctx context.Context, uploadID string, txn model.ExternalTransaction) error {
	return s.datasource.RecordExternalTransaction(ctx, &txn, uploadID)
}

func (l *Blnk) postReconciliationActions(_ context.Context, reconciliation model.Reconciliation) {
	go func() {
		err := l.queue.queueIndexData(reconciliation.ReconciliationID, "reconciliations", reconciliation)
		if err != nil {
			notification.NotifyError(err)
		}
	}()
}

func (s *Blnk) StartReconciliation(ctx context.Context, uploadID string, strategy string, groupCriteria string, matchingRuleIDs []string, isDryRun bool) (string, error) {
	reconciliationID := model.GenerateUUIDWithSuffix("recon")
	reconciliation := model.Reconciliation{
		ReconciliationID: reconciliationID,
		UploadID:         uploadID,
		Status:           StatusStarted,
		StartedAt:        time.Now(),
		IsDryRun:         isDryRun,
	}

	if err := s.datasource.RecordReconciliation(ctx, &reconciliation); err != nil {
		return "", err
	}

	// Start the reconciliation process in a goroutine
	go func() {
		err := s.processReconciliation(context.Background(), reconciliation, strategy, groupCriteria, matchingRuleIDs)
		if err != nil {
			log.Printf("Error in reconciliation process: %v", err)
			err := s.datasource.UpdateReconciliationStatus(context.Background(), reconciliationID, StatusFailed, 0, 0)
			if err != nil {
				log.Printf("Error updating reconciliation status: %v", err)
			}
		}
	}()

	return reconciliationID, nil
}

func (s *Blnk) matchesRules(externalTxn *model.Transaction, groupTxn model.Transaction, rules []model.MatchingRule) bool {
	for _, rule := range rules {
		allCriteriaMet := true
		for _, criteria := range rule.Criteria {
			var criterionMet bool
			switch criteria.Field {
			case "amount":
				criterionMet = s.matchesGroupAmount(externalTxn.Amount, groupTxn.Amount, criteria)
			case "date":
				criterionMet = s.matchesGroupDate(externalTxn.CreatedAt, groupTxn.CreatedAt, criteria)
			case "description":
				criterionMet = s.matchesString(externalTxn.Description, groupTxn.Description, criteria)
			case "reference":
				criterionMet = s.matchesString(externalTxn.Reference, groupTxn.Reference, criteria)
			case "currency":
				criterionMet = s.matchesCurrency(externalTxn.Currency, groupTxn.Currency, criteria)
			}
			if !criterionMet {
				allCriteriaMet = false
				break
			}
		}
		if allCriteriaMet {
			return true
		}
	}
	return false
}

func (s *Blnk) loadReconciliationProgress(ctx context.Context, reconciliationID string) (model.ReconciliationProgress, error) {
	return s.datasource.LoadReconciliationProgress(ctx, reconciliationID)
}

func (s *Blnk) processReconciliation(ctx context.Context, reconciliation model.Reconciliation, strategy string, groupCriteria string, matchingRuleIDs []string) error {
	if err := s.updateReconciliationStatus(ctx, reconciliation.ReconciliationID, StatusInProgress); err != nil {
		return fmt.Errorf("failed to update reconciliation status: %w", err)
	}

	matchingRules, err := s.getMatchingRules(ctx, matchingRuleIDs)
	if err != nil {
		return fmt.Errorf("failed to get matching rules: %w", err)
	}

	progress, err := s.initializeReconciliationProgress(ctx, reconciliation.ReconciliationID)
	if err != nil {
		return fmt.Errorf("failed to initialize reconciliation progress: %w", err)
	}

	reconciler := s.createReconciler(strategy, groupCriteria, matchingRules)

	processor := s.createTransactionProcessor(reconciliation, progress, reconciler)

	err = s.processTransactions(ctx, reconciliation.UploadID, processor)
	if err != nil {
		return fmt.Errorf("failed to process transactions: %w", err)
	}

	matched, unmatched := processor.getResults()

	return s.finalizeReconciliation(ctx, reconciliation, matched, unmatched)
}

func (s *Blnk) updateReconciliationStatus(ctx context.Context, reconciliationID, status string) error {
	return s.datasource.UpdateReconciliationStatus(ctx, reconciliationID, status, 0, 0)
}

func (s *Blnk) initializeReconciliationProgress(ctx context.Context, reconciliationID string) (model.ReconciliationProgress, error) {
	progress, err := s.loadReconciliationProgress(ctx, reconciliationID)
	if err != nil {
		log.Printf("Error loading reconciliation progress: %v", err)
		return model.ReconciliationProgress{}, nil
	}
	return progress, nil
}

func (s *Blnk) createReconciler(strategy string, groupCriteria string, matchingRules []model.MatchingRule) reconciler {
	return func(ctx context.Context, txns []*model.Transaction) ([]model.Match, []string) {
		switch strategy {
		case "one_to_one":
			return s.oneToOneReconciliation(ctx, txns, matchingRules)
		case "one_to_many":
			return s.groupToNReconciliation(ctx, txns, groupCriteria, matchingRules, false)
		case "many_to_one":
			return s.groupToNReconciliation(ctx, txns, groupCriteria, matchingRules, true)
		default:
			log.Printf("Unsupported reconciliation strategy: %s", strategy)
			return nil, nil
		}
	}
}

func (s *Blnk) createTransactionProcessor(reconciliation model.Reconciliation, progress model.ReconciliationProgress, reconciler func(ctx context.Context, txns []*model.Transaction) ([]model.Match, []string)) *transactionProcessor {
	return &transactionProcessor{
		reconciliation:    reconciliation,
		progress:          progress,
		reconciler:        reconciler,
		datasource:        s.datasource,
		progressSaveCount: 100,
	}
}

func (tp *transactionProcessor) process(ctx context.Context, txn *model.Transaction) error {
	batchMatches, batchUnmatched := tp.reconciler(ctx, []*model.Transaction{txn})

	tp.matches = append(tp.matches, batchMatches...)
	tp.unmatched = append(tp.unmatched, batchUnmatched...)

	if !tp.reconciliation.IsDryRun {
		if err := tp.datasource.RecordMatches(ctx, tp.reconciliation.ReconciliationID, batchMatches); err != nil {
			return err
		}

		if err := tp.datasource.RecordUnmatched(ctx, tp.reconciliation.ReconciliationID, batchUnmatched); err != nil {
			return err
		}
	}

	tp.progress.LastProcessedExternalTxnID = txn.TransactionID
	tp.progress.ProcessedCount++

	if tp.progress.ProcessedCount%tp.progressSaveCount == 0 {
		if err := tp.datasource.SaveReconciliationProgress(ctx, tp.reconciliation.ReconciliationID, tp.progress); err != nil {
			log.Printf("Error saving reconciliation progress: %v", err)
		}
	}

	return nil
}

func (tp *transactionProcessor) getResults() (int, int) {
	return len(tp.matches), len(tp.unmatched)
}

func (s *Blnk) processTransactions(ctx context.Context, uploadID string, processor *transactionProcessor) error {
	processedCount := 0
	_, err := s.ProcessTransactionInBatches(
		ctx,
		uploadID,
		0,
		s.getExternalTransactionsPaginated,
		func(ctx context.Context, txns <-chan *model.Transaction, results chan<- BatchJobResult, wg *sync.WaitGroup, _ float64) {
			defer wg.Done()
			for txn := range txns {
				if err := processor.process(ctx, txn); err != nil {
					log.Printf("Error processing transaction %s: %v", txn.TransactionID, err)
					results <- BatchJobResult{Error: err}
					return
				}
				processedCount++
				if processedCount%10 == 0 {
					log.Printf("Processed %d transactions", processedCount)
				}
				results <- BatchJobResult{}

			}
		},
	)
	log.Printf("Total transactions processed: %d", processedCount)
	return err
}

func (s *Blnk) finalizeReconciliation(ctx context.Context, reconciliation model.Reconciliation, matchCount, unmatchedCount int) error {
	reconciliation.Status = StatusCompleted
	reconciliation.UnmatchedTransactions = unmatchedCount
	reconciliation.MatchedTransactions = matchCount
	reconciliation.CompletedAt = ptr.Time(time.Now())

	log.Printf("Finalizing reconciliation. Matches: %d, Unmatched: %d", matchCount, unmatchedCount)

	if !reconciliation.IsDryRun {
		s.postReconciliationActions(ctx, reconciliation)
	} else {
		log.Printf("Dry run completed. Matches: %d, Unmatched: %d", matchCount, unmatchedCount)
	}

	err := s.datasource.UpdateReconciliationStatus(ctx, reconciliation.ReconciliationID, StatusCompleted, matchCount, unmatchedCount)
	if err != nil {
		log.Printf("Error updating reconciliation status: %v", err)
		return err
	}

	log.Printf("Reconciliation %s completed. Total matches: %d, Total unmatched: %d", reconciliation.ReconciliationID, matchCount, unmatchedCount)

	return nil
}

func (s *Blnk) oneToOneReconciliation(ctx context.Context, externalTxns []*model.Transaction, matchingRules []model.MatchingRule) ([]model.Match, []string) {
	var matches []model.Match
	var unmatched []string

	matchChan := make(chan model.Match, len(externalTxns))
	unmatchedChan := make(chan string, len(externalTxns))
	var wg sync.WaitGroup

	for _, externalTxn := range externalTxns {
		wg.Add(1)
		go func(extTxn *model.Transaction) {
			defer wg.Done()
			err := s.findMatchingInternalTransaction(ctx, extTxn, matchingRules, matchChan, unmatchedChan)
			if err != nil {
				unmatchedChan <- extTxn.TransactionID
				log.Printf("No match found for external transaction %s: %v", extTxn.TransactionID, err)
			}
		}(externalTxn)
	}

	// Close channels after all goroutines are done
	go func() {
		wg.Wait()
		close(matchChan)
		close(unmatchedChan)
	}()

	for match := range matchChan {
		matches = append(matches, match)
	}
	for unmatchedID := range unmatchedChan {
		unmatched = append(unmatched, unmatchedID)
	}

	return matches, unmatched
}

func (s *Blnk) groupToNReconciliation(ctx context.Context, externalTxns []*model.Transaction, groupCriteria string, matchingRules []model.MatchingRule, isExternalGrouped bool) ([]model.Match, []string) {
	groupedTxns, singleTxns := s.prepareTransactions(ctx, externalTxns, groupCriteria, isExternalGrouped)
	matches, unmatched := s.matchTransactions(singleTxns, groupedTxns, matchingRules, isExternalGrouped)
	return matches, unmatched
}

func (s *Blnk) prepareTransactions(ctx context.Context, externalTxns []*model.Transaction, groupCriteria string, isExternalGrouped bool) (map[string][]*model.Transaction, []*model.Transaction) {
	const batchSize = 100

	if isExternalGrouped {
		return s.groupExternalTransactions(externalTxns), s.fetchAllInternalTransactions(ctx, batchSize)
	}
	return s.fetchAndGroupInternalTransactions(ctx, groupCriteria, batchSize), externalTxns
}

func (s *Blnk) matchTransactions(singleTxns []*model.Transaction, groupedTxns map[string][]*model.Transaction, matchingRules []model.MatchingRule, isExternalGrouped bool) ([]model.Match, []string) {
	var matches []model.Match
	var unmatched []string

	groupMap := s.buildGroupMap(groupedTxns)

	matches, unmatched = s.processGroupedTransactions(singleTxns, groupedTxns, groupMap, matchingRules, isExternalGrouped)
	return matches, unmatched
}

func (s *Blnk) processGroupedTransactions(singleTxns []*model.Transaction, groupedTxns map[string][]*model.Transaction, groupMap map[string]bool, matchingRules []model.MatchingRule, isExternalGrouped bool) ([]model.Match, []string) {
	const batchSize = 100
	var matches []model.Match
	var unmatched []string

	var wg sync.WaitGroup
	matchChan := make(chan model.Match, len(singleTxns))
	unmatchedChan := make(chan string, len(singleTxns))

	for i := 0; i < len(singleTxns); i += batchSize {
		end := i + batchSize
		if end > len(singleTxns) {
			end = len(singleTxns)
		}

		wg.Add(1)
		go func(batch []*model.Transaction) {
			defer wg.Done()
			for _, singleTxn := range batch {
				_ = s.matchSingleTransaction(singleTxn, groupedTxns, groupMap, matchingRules, isExternalGrouped, matchChan)
			}
		}(singleTxns[i:end])
	}

	go func() {
		wg.Wait()
		close(matchChan)
		close(unmatchedChan)
	}()

	for match := range matchChan {
		matches = append(matches, match)
	}
	for unmatchedID := range unmatchedChan {
		unmatched = append(unmatched, unmatchedID)
	}
	return matches, unmatched
}

func (s *Blnk) matchSingleTransaction(singleTxn *model.Transaction, groupedTxns map[string][]*model.Transaction, groupMap map[string]bool, matchingRules []model.MatchingRule, isExternalGrouped bool, matchChan chan model.Match) bool {
	for groupKey := range groupMap {
		if s.matchesGroup(singleTxn, groupedTxns[groupKey], matchingRules) {
			for _, groupedTxn := range groupedTxns[groupKey] {
				var externalID, internalID string
				if isExternalGrouped {
					externalID = groupedTxn.TransactionID
					internalID = singleTxn.TransactionID
				} else {
					externalID = singleTxn.TransactionID
					internalID = groupedTxn.TransactionID
				}
				matchChan <- model.Match{
					ExternalTransactionID: externalID,
					InternalTransactionID: internalID,
					Amount:                groupedTxn.Amount,
					Date:                  groupedTxn.CreatedAt,
				}
			}
			delete(groupMap, groupKey)
			break
		}
	}
	return true
}

func (s *Blnk) buildGroupMap(groupedTxns map[string][]*model.Transaction) map[string]bool {
	groupMap := make(map[string]bool)
	for key := range groupedTxns {
		groupMap[key] = true
	}
	return groupMap
}

func (s *Blnk) fetchAllInternalTransactions(ctx context.Context, batchSize int) []*model.Transaction {
	var allTxns []*model.Transaction
	offset := int64(0)
	for {
		txns, err := s.getInternalTransactionsPaginated(ctx, "", batchSize, offset)
		if err != nil {
			log.Printf("Error fetching internal transactions: %v", err)
			break
		}
		if len(txns) == 0 {
			break
		}
		allTxns = append(allTxns, txns...)
		offset += int64(len(txns))
	}
	return allTxns
}

func (s *Blnk) fetchAndGroupInternalTransactions(ctx context.Context, groupingCriteria string, batchSize int) map[string][]*model.Transaction {
	groupedTxns := make(map[string][]*model.Transaction)
	offset := int64(0)
	for {
		txns, err := s.groupInternalTransactions(ctx, groupingCriteria, batchSize, offset)
		if err != nil {
			log.Printf("Error grouping internal transactions: %v", err)
			break
		}
		if len(txns) == 0 {
			break
		}
		for key, group := range txns {
			groupedTxns[key] = append(groupedTxns[key], group...)
		}
		offset += int64(batchSize)
	}
	return groupedTxns
}

func (s *Blnk) groupInternalTransactions(ctx context.Context, groupingCriteria string, batchSize int, offset int64) (map[string][]*model.Transaction, error) {
	return s.datasource.GroupTransactions(ctx, groupingCriteria, batchSize, offset)
}

func (s *Blnk) groupExternalTransactions(externalTxns []*model.Transaction) map[string][]*model.Transaction {
	grouped := make(map[string][]*model.Transaction)
	const timeWindow = 2 * time.Hour // Adjust this value as needed

	for _, txn := range externalTxns {
		var key string
		var assigned bool

		// Check existing groups for a close match
		for existingKey, group := range grouped {
			for _, groupTxn := range group {
				if txn.Currency == groupTxn.Currency &&
					math.Abs(txn.CreatedAt.Sub(groupTxn.CreatedAt).Hours()) <= timeWindow.Hours() {
					key = existingKey
					assigned = true
					break
				}
			}
			if assigned {
				break
			}
		}

		// If no close match found, create a new group
		if !assigned {
			key = fmt.Sprintf("%s_%s", txn.Currency, txn.CreatedAt.Format("2006-01-02T15:04:05"))
		}

		grouped[key] = append(grouped[key], txn)
	}

	return grouped
}

func (s *Blnk) findMatchingInternalTransaction(ctx context.Context, externalTxn *model.Transaction, matchingRules []model.MatchingRule, matchChan chan model.Match, unMatchChan chan string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	matchFound := false
	_, err := s.ProcessTransactionInBatches(
		ctx,
		externalTxn.TransactionID,
		externalTxn.Amount,
		s.getInternalTransactionsPaginated,
		func(ctx context.Context, jobs <-chan *model.Transaction, results chan<- BatchJobResult, wg *sync.WaitGroup, amount float64) {
			defer wg.Done()
			for internalTxn := range jobs {
				select {
				case <-ctx.Done():
					return
				default:
					if s.matchesRules(externalTxn, *internalTxn, matchingRules) {
						fmt.Println("Match found")
						matchChan <- model.Match{
							ExternalTransactionID: externalTxn.TransactionID,
							InternalTransactionID: internalTxn.TransactionID,
							Amount:                externalTxn.Amount,
							Date:                  externalTxn.CreatedAt,
						}
						matchFound = true
						cancel()
						return
					}
				}
			}
		},
	)
	if err != nil && err != context.Canceled {
		return err
	}

	if !matchFound {
		select {
		case unMatchChan <- externalTxn.TransactionID:
		default:
			return fmt.Errorf("failed to send unmatched transaction ID to channel")
		}
	}

	return nil
}

func (s *Blnk) getExternalTransactionsPaginated(ctx context.Context, uploadID string, limit int, offset int64) ([]*model.Transaction, error) {
	log.Printf("Fetching external transactions: uploadID=%s, limit=%d, offset=%d", uploadID, limit, offset)
	externalTransaction, err := s.datasource.GetExternalTransactionsPaginated(ctx, uploadID, limit, int64(offset))
	if err != nil {
		log.Printf("Error fetching external transactions: %v", err)
		return nil, err
	}
	log.Printf("Fetched %d external transactions", len(externalTransaction))
	transactions := make([]*model.Transaction, len(externalTransaction))

	for i, txn := range externalTransaction {
		transactions[i] = txn.ToInternalTransaction()
	}
	return transactions, nil
}

func (s *Blnk) getInternalTransactionsPaginated(ctx context.Context, id string, limit int, offset int64) ([]*model.Transaction, error) {
	return s.datasource.GetTransactionsPaginated(ctx, "", limit, offset)
}

func (s *Blnk) matchesGroup(externalTxn *model.Transaction, group []*model.Transaction, matchingRules []model.MatchingRule) bool {
	var totalAmount float64
	var minDate, maxDate time.Time
	descriptions := make([]string, 0, len(group))
	references := make([]string, 0, len(group))
	currencies := make(map[string]bool)
	for i, internalTxn := range group {
		totalAmount += internalTxn.Amount

		if i == 0 || internalTxn.CreatedAt.Before(minDate) {
			minDate = internalTxn.CreatedAt
		}
		if i == 0 || internalTxn.CreatedAt.After(maxDate) {
			maxDate = internalTxn.CreatedAt
		}

		descriptions = append(descriptions, internalTxn.Description)
		references = append(references, internalTxn.Reference)
		currencies[internalTxn.Currency] = true
	}

	// Create a virtual transaction representing the group
	groupTxn := model.Transaction{
		Amount:      totalAmount,
		CreatedAt:   minDate, //  use the earliest date in the group
		Description: strings.Join(descriptions, " | "),
		Reference:   strings.Join(references, " | "),
		Currency:    s.dominantCurrency(currencies),
	}

	return s.matchesRules(externalTxn, groupTxn, matchingRules)
}

func (s *Blnk) dominantCurrency(currencies map[string]bool) string {
	if len(currencies) == 1 {
		for currency := range currencies {
			return currency
		}
	}
	return "MIXED"
}

func (s *Blnk) CreateMatchingRule(ctx context.Context, rule model.MatchingRule) (*model.MatchingRule, error) {
	rule.RuleID = model.GenerateUUIDWithSuffix("rule")
	rule.CreatedAt = time.Now()
	rule.UpdatedAt = time.Now()

	err := s.validateRule(&rule)
	if err != nil {
		return nil, err
	}

	err = s.datasource.RecordMatchingRule(ctx, &rule)
	if err != nil {
		return nil, err
	}

	return &rule, nil
}

func (s *Blnk) GetMatchingRule(ctx context.Context, id string) (*model.MatchingRule, error) {
	rule, err := s.datasource.GetMatchingRule(ctx, id)
	if err != nil {
		return nil, err
	}
	return rule, nil
}

func (s *Blnk) UpdateMatchingRule(ctx context.Context, rule model.MatchingRule) (*model.MatchingRule, error) {
	existingRule, err := s.GetMatchingRule(ctx, rule.RuleID)
	if err != nil {
		return nil, err
	}

	rule.CreatedAt = existingRule.CreatedAt
	rule.UpdatedAt = time.Now()

	err = s.validateRule(&rule)
	if err != nil {
		return nil, err
	}

	err = s.datasource.UpdateMatchingRule(ctx, &rule)
	if err != nil {
		return nil, err
	}

	return &rule, nil
}

func (s *Blnk) DeleteMatchingRule(ctx context.Context, id string) error {
	return s.datasource.DeleteMatchingRule(ctx, id)
}

func (s *Blnk) ListMatchingRules(ctx context.Context) ([]*model.MatchingRule, error) {
	return s.datasource.GetMatchingRules(ctx)
}

func (s *Blnk) validateRule(rule *model.MatchingRule) error {
	if rule.Name == "" {
		return errors.New("rule name is required")
	}

	if len(rule.Criteria) == 0 {
		return errors.New("at least one matching criteria is required")
	}

	for _, criteria := range rule.Criteria {
		if criteria.Field == "" || criteria.Operator == "" {
			return errors.New("field and operator are required for each criteria")
		}

		// Validate operator
		validOperators := []string{"equals", "greater_than", "less_than", "contains"}
		if !contains(validOperators, criteria.Operator) {
			return errors.New("invalid operator")
		}

		// Validate field
		validFields := []string{"amount", "date", "description", "reference", "currency"}
		if !contains(validFields, criteria.Field) {
			return errors.New("invalid field")
		}

		// Validate Drift
		if criteria.Operator == "equals" {
			if criteria.Field == "amount" {
				if criteria.AllowableDrift < 0 || criteria.AllowableDrift > 100 {
					return errors.New("drift for amount must be between 0 and 100 (percentage)")
				}
			} else if criteria.Field == "date" {
				if criteria.AllowableDrift < 0 {
					return errors.New("drift for date must be non-negative (seconds)")
				}
			}
		}
	}

	return nil
}

func (s *Blnk) getMatchingRules(ctx context.Context, matchingRuleIDs []string) ([]model.MatchingRule, error) {
	var rules []model.MatchingRule
	for _, id := range matchingRuleIDs {
		rule, err := s.GetMatchingRule(ctx, id)
		if err != nil {
			return nil, err
		}
		rules = append(rules, *rule)
	}
	return rules, nil
}

func (s *Blnk) matchesString(externalValue, internalValue string, criteria model.MatchingCriteria) bool {
	switch criteria.Operator {
	case "equals":
		// Split the internal value and check if any part matches exactly
		for _, part := range strings.Split(internalValue, " | ") {
			if strings.EqualFold(externalValue, part) {
				return true
			}
		}
		return false
	case "contains":
		// Check if the external value is contained in any part of the internal value
		for _, part := range strings.Split(internalValue, " | ") {
			if s.partialMatch(externalValue, part, criteria.AllowableDrift) {
				return true
			}
		}
		return false
	}
	return false
}

func (s *Blnk) partialMatch(str1, str2 string, allowableDrift float64) bool {
	// Convert strings to lowercase for case-insensitive comparison
	str1 = strings.ToLower(str1)
	str2 = strings.ToLower(str2)

	// Check if either string contains the other
	if strings.Contains(str1, str2) || strings.Contains(str2, str1) {
		return true
	}

	// Calculate Levenshtein distance
	distance := levenshtein.DistanceForStrings([]rune(str1), []rune(str2), levenshtein.DefaultOptions)

	// Calculate the maximum allowed distance based on the length of the longer string and the allowable drift
	maxLength := float64(max(len(str1), len(str2)))
	maxAllowedDistance := int(maxLength * (allowableDrift / 100))

	// Return true if the distance is within the allowed range
	return distance <= maxAllowedDistance
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *Blnk) matchesCurrency(externalValue, internalValue string, criteria model.MatchingCriteria) bool {
	if internalValue == "MIXED" {
		// Decide how to handle mixed currencies. Maybe always return true, or implement a more complex logic
		return true
	}
	return s.matchesString(externalValue, internalValue, criteria)
}

func (s *Blnk) matchesGroupAmount(externalAmount, groupAmount float64, criteria model.MatchingCriteria) bool {
	switch criteria.Operator {
	case "equals":
		allowableDrift := groupAmount * criteria.AllowableDrift
		return math.Abs(externalAmount-groupAmount) <= allowableDrift
	case "greater_than":
		return externalAmount > groupAmount
	case "less_than":
		return externalAmount < groupAmount
	}
	return false
}

func (s *Blnk) matchesGroupDate(externalDate, groupEarliestDate time.Time, criteria model.MatchingCriteria) bool {
	switch criteria.Operator {
	case "equals":
		difference := externalDate.Sub(groupEarliestDate)
		return math.Abs(float64(difference/time.Second)) <= criteria.AllowableDrift
	case "after":
		return externalDate.After(groupEarliestDate)
	case "before":
		return externalDate.Before(groupEarliestDate)
	}
	return false
}
