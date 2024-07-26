package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jerry-enebeli/blnk/model"
	"github.com/sirupsen/logrus"
)

// UploadExternalData handles the upload of external transaction data
func (a Api) UploadExternalData(c *gin.Context) {
	source := c.PostForm("source")
	file, _, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "File upload failed"})
		return
	}
	defer file.Close()

	uploadID, err := a.blnk.UploadExternalData(c.Request.Context(), source, file)
	if err != nil {
		logrus.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process upload"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"upload_id": uploadID})
}

// StartReconciliation initiates a new reconciliation process
func (a Api) StartReconciliation(c *gin.Context) {
	var req struct {
		UploadID         string                 `json:"upload_id" binding:"required"`
		Strategy         string                 `json:"strategy" binding:"required"`
		GroupingCriteria map[string]interface{} `json:"grouping_criteria"`
		MatchingRuleIDs  []string               `json:"matching_rule_ids" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	reconciliationID, err := a.blnk.StartReconciliation(c.Request.Context(), req.UploadID, req.Strategy, req.GroupingCriteria, req.MatchingRuleIDs)
	if err != nil {
		logrus.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start reconciliation"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"reconciliation_id": reconciliationID})
}

// GetReconciliationStatus retrieves the status of a reconciliation process
func (a Api) GetReconciliationStatus(c *gin.Context) {
	reconciliationID := c.Param("id")
	if reconciliationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Reconciliation ID is required"})
		return
	}

	status, err := a.blnk.GetReconciliationStatus(c.Request.Context(), reconciliationID)
	if err != nil {
		logrus.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve reconciliation status"})
		return
	}

	c.JSON(http.StatusOK, status)
}

// GetReconciliationResults retrieves the results of a completed reconciliation
func (a Api) GetReconciliationResults(c *gin.Context) {
	reconciliationID := c.Param("id")
	if reconciliationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Reconciliation ID is required"})
		return
	}

	results, err := a.blnk.GetReconciliationResults(c.Request.Context(), reconciliationID)
	if err != nil {
		logrus.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve reconciliation results"})
		return
	}

	c.JSON(http.StatusOK, results)
}

// CreateMatchingRule creates a new matching rule
func (a Api) CreateMatchingRule(c *gin.Context) {
	var rule model.MatchingRule
	if err := c.ShouldBindJSON(&rule); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	createdRule, err := a.blnk.CreateMatchingRule(c.Request.Context(), rule)
	if err != nil {
		logrus.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create matching rule"})
		return
	}

	c.JSON(http.StatusCreated, createdRule)
}

// UpdateMatchingRule updates an existing matching rule
func (a Api) UpdateMatchingRule(c *gin.Context) {
	ruleID := c.Param("id")
	if ruleID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Matching Rule ID is required"})
		return
	}

	var rule model.MatchingRule
	if err := c.ShouldBindJSON(&rule); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	rule.RuleID = ruleID
	updatedRule, err := a.blnk.UpdateMatchingRule(c.Request.Context(), rule)
	if err != nil {
		logrus.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update matching rule"})
		return
	}

	c.JSON(http.StatusOK, updatedRule)
}

// DeleteMatchingRule deletes a matching rule
func (a Api) DeleteMatchingRule(c *gin.Context) {
	ruleID := c.Param("id")
	if ruleID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Matching Rule ID is required"})
		return
	}

	err := a.blnk.DeleteMatchingRule(c.Request.Context(), ruleID)
	if err != nil {
		logrus.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete matching rule"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Matching rule deleted successfully"})
}