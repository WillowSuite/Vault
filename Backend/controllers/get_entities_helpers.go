package controllers

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"willowsuite-vault/infra/logger"
)

func getEntitiesParseQueryParams(values url.Values) (int, int, string, []string, error) {
	offsetString := values.Get("offset")
	limitString := values.Get("limit")
	search := values.Get("search")
	filterString := values.Get("filter")
	filters := []string{}

	if offsetString == "" {
		offsetString = "0"
	}

	if limitString == "" {
		limitString = "20"
	}

	offset, err := strconv.Atoi(offsetString)
	if err != nil {
		logger.Errorf("Error converting offset to int: %v", err)
		return 0, 0, "", filters, err
	}

	limit, err := strconv.Atoi(limitString)
	if err != nil {
		logger.Errorf("Error converting limit to int: %v", err)
		return 0, 0, "", filters, err
	}

	if offset < 0 {
		err = errors.New("offset must be positive")
		logger.Errorf("Error: %v", err)
		return 0, 0, "", filters, err
	}

	if limit < 0 {
		err = errors.New("limit must be positive")
		logger.Errorf("Error: %v", err)
		return 0, 0, "", filters, err
	}

	if filterString != "" {
		filters = strings.Split(filterString, ",")
	}

	return offset, limit, search, filters, nil
}
