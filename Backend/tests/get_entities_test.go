// Package tests is where all of out unit tests are described.
package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"testing"
	"time"
	"willowsuite-vault/controllers"
	"willowsuite-vault/models"
	"willowsuite-vault/repository"
	"willowsuite-vault/tests/mocks"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/go-redis/redismock/v9"
	"github.com/golang/mock/gomock"
)

type getEntitiesSingleResponse struct {
	Message string                     `json:"message"`
	Data    models.GetEntitiesResponse `json:"data"`
}

type getEntitiesTestCase struct {
	testName       string
	testUser       string
	testCacheHit   bool
	testValidInput bool
	offset         string
	limit          string
}

var getEntitiesEndpoint = "/v1/entities"

func setupGetEntitiesTest(t *testing.T, userName string) (*http.Client, *httptest.Server, sqlmock.Sqlmock, redismock.ClientMock) {
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	postgres, mockDB := mocks.NewMockDB()
	redis, mockCache := redismock.NewClientMock()
	handler := controllers.Handler{
		Repository:    &repository.Repository{Database: postgres, Cache: redis},
		CognitoClient: mocks.NewMockCognitoClient(ctrl),
		TokenHelper:   mocks.NewMockTokenHelper(ctrl),
	}

	r := chi.NewRouter()
	r.Use(mocks.MockJWTMiddleware(userName))
	r.Get(getEntitiesEndpoint, handler.GetEntities)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	return &http.Client{}, srv, mockDB, mockCache
}

func setupGetEntitiesCacheMissMockExpectations(mockDB *sqlmock.Sqlmock, mockCache redismock.ClientMock, userName string, offset string, limit string) {
	if offset == "" {
		offset = "0"
	}

	if limit == "" {
		limit = "20"
	}

	cacheKey := fmt.Sprintf(`{"CacheKey":{"User":"%s","Function":"GetAllEntities"},"Offset":"%s","Limit":"%s","Search":"","Filters":[]}`, userName, offset, limit)
	countCacheKey := fmt.Sprintf(`{"CacheKey":{"User":"%s","Function":"CountEntities"},"Search":"","Filters":[]}`, userName)

	expectedMainSQL := fmt.Sprintf(`
		(SELECT 1 AS tableWeight, created_at, 'building' AS category, id, name, notes, address, 0 AS parent_id, ' ' AS parent_category FROM buildings WHERE user_id = $1 AND deleted_at IS NULL ORDER BY created_at ASC LIMIT $2)
    	UNION ALL
    	(SELECT 2 AS tableWeight, created_at, 'room' AS category, id, name, notes, '' AS address, parent_id, parent_category FROM rooms WHERE user_id = $3 AND deleted_at IS NULL ORDER BY created_at ASC LIMIT $4)
    	UNION ALL
    	(SELECT 3 AS tableWeight, created_at, 'shelving_unit' AS category, id, name, notes, '' AS address, parent_id, parent_category FROM shelving_units WHERE user_id = $5 AND deleted_at IS NULL ORDER BY created_at ASC LIMIT $6)
    	UNION ALL
    	(SELECT 4 AS tableWeight, created_at, 'shelf' AS category, id, name, notes, '' AS address, parent_id, parent_category FROM shelves WHERE user_id = $7 AND deleted_at IS NULL ORDER BY created_at ASC LIMIT $8)
    	UNION ALL
    	(SELECT 5 AS tableWeight, created_at, 'container' AS category, id, name, notes, '' AS address, parent_id, parent_category FROM containers WHERE user_id = $9 AND deleted_at IS NULL ORDER BY created_at ASC LIMIT $10)
    	UNION ALL
    	(SELECT 6 AS tableWeight, created_at, 'item' AS category, id, name, notes, '' AS address, parent_id, parent_category FROM items WHERE user_id = $11 AND deleted_at IS NULL ORDER BY created_at ASC LIMIT $12)
    	ORDER BY tableWeight OFFSET $13 LIMIT $14`)

	expectedCountSQL := `SELECT (SELECT COUNT(*) FROM buildings WHERE user_id = $1 AND deleted_at IS NULL ) +
                            (SELECT COUNT(*) FROM rooms WHERE user_id = $2 AND deleted_at IS NULL ) +
                            (SELECT COUNT(*) FROM shelving_units WHERE user_id = $3 AND deleted_at IS NULL ) +
                            (SELECT COUNT(*) FROM shelves WHERE user_id = $4 AND deleted_at IS NULL ) +
                            (SELECT COUNT(*) FROM containers WHERE user_id = $5 AND deleted_at IS NULL ) +
                            (SELECT COUNT(*) FROM items WHERE user_id = $6 AND deleted_at IS NULL ) AS EntityCount`

	mockCache.ExpectGet(cacheKey).RedisNil()
	mockCache.ExpectGet(countCacheKey).RedisNil()

	(*mockDB).ExpectQuery(regexp.QuoteMeta(expectedMainSQL)).
		WithArgs(userName, limit, userName, limit, userName, limit, userName, limit, userName, limit, userName, limit, offset, limit).
		WillReturnRows(sqlmock.NewRows([]string{"tableWeight", "created_at", "category", "id", "name", "notes", "address", "parent_id", "parent_category"}).
			AddRow(1, time.Now(), "building", 1, "Building 1", " ", " ", 0, " ").
			AddRow(2, time.Now(), "room", 1, "Room 1", " ", " ", 1, "building").
			AddRow(3, time.Now(), "shelving_unit", 1, "Shelving Unit 1", " ", " ", 1, "room").
			AddRow(4, time.Now(), "shelf", 1, "Shelf 1", " ", " ", 1, "shelving_unit").
			AddRow(5, time.Now(), "container", 1, "Container 1", " ", " ", 1, "shelf").
			AddRow(6, time.Now(), "item", 2, "Item 2", " ", " ", 1, "container"))

	// Recursive parent expectations - room
	expectBuilding(mockDB, userName)

	// shelving unit
	expectRoom(mockDB, userName)
	expectBuilding(mockDB, userName)

	// shelf
	expectUnit(mockDB, userName)
	expectRoom(mockDB, userName)
	expectBuilding(mockDB, userName)

	// container
	expectShelf(mockDB, userName)
	expectUnit(mockDB, userName)
	expectRoom(mockDB, userName)
	expectBuilding(mockDB, userName)

	// item
	expectContainer(mockDB, userName)
	expectShelf(mockDB, userName)
	expectUnit(mockDB, userName)
	expectRoom(mockDB, userName)
	expectBuilding(mockDB, userName)

	(*mockDB).ExpectQuery(regexp.QuoteMeta(expectedCountSQL)).
		WithArgs(userName, userName, userName, userName, userName, userName).
		WillReturnRows(sqlmock.NewRows([]string{"EntityCount"}).AddRow(12))

	// Expect set for main entities cache key
	// mockCache.Regexp().ExpectSet(regexp.QuoteMeta(cacheKey), ".*", 5*time.Minute).SetVal("OK")

	// Expect set for count cache key
	// mockCache.Regexp().ExpectSet(countCacheKey, ".*", 5*time.Minute).SetVal("OK")
}

func setupGetEntitiesCacheHitMockExpectations(mockCache redismock.ClientMock, userName string, offset string, limit string) {
	if offset == "" {
		offset = "0"
	}

	if limit == "" {
		limit = "20"
	}

	cacheKey := fmt.Sprintf(`{"CacheKey":{"User":"%s","Function":"GetAllEntities"},"Offset":"%s","Limit":"%s","Search":"","Filters":[]}`, userName, offset, limit)
	countCacheKey := fmt.Sprintf(`{"CacheKey":{"User":"%s","Function":"CountEntities"},"Search":"","Filters":[]}`, userName)

	mockCache.ExpectGet(cacheKey).SetVal(`[
											{"ID":36,"Name":"Home","Category":"building","Location":" ","Notes":"Some test notes for the building."},
											{"ID":11,"Name":"Another Test Room","Category":"room","Location":" ","Notes":""},
											{"ID":13,"Name":"Test Unit","Category":"shelving_unit","Location":" ","Notes":""},
											{"ID":10,"Name":"Test Shelf","Category":"shelf","Location":" ","Notes":"Just some test notes for the test shelf."},
											{"ID":11,"Name":"Test Container","Category":"container","Location":" ","Notes":"Just a test container notes."},
											{"ID":85,"Name":"Test Entity","Category":"item","Location":" ","Notes":"Maybe."}
										]`)

	mockCache.ExpectGet(countCacheKey).SetVal("6")
}

func expectContainer(mockDB *sqlmock.Sqlmock, userName string) {
	(*mockDB).ExpectQuery(regexp.QuoteMeta(`SELECT * FROM "containers" WHERE user_id = $1 AND "containers"."deleted_at" IS NULL AND "containers"."id" = $2 ORDER BY "containers"."id" LIMIT 1`)).
		WithArgs(userName, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "notes", "created_at", "updated_at", "user_id", "parent_id", "parent_category"}).
			AddRow(1, "Container 1", "test notes", time.Now(), time.Now(), userName, 1, "shelf"))
}

func expectShelf(mockDB *sqlmock.Sqlmock, userName string) {
	(*mockDB).ExpectQuery(regexp.QuoteMeta(`SELECT * FROM "shelves" WHERE user_id = $1 AND "shelves"."deleted_at" IS NULL AND "shelves"."id" = $2 ORDER BY "shelves"."id" LIMIT 1`)).
		WithArgs(userName, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "notes", "created_at", "updated_at", "user_id", "parent_id", "parent_category"}).
			AddRow(1, "Shelf 1", "test notes", time.Now(), time.Now(), userName, 1, "shelving_unit"))
}

func expectUnit(mockDB *sqlmock.Sqlmock, userName string) {
	(*mockDB).ExpectQuery(regexp.QuoteMeta(`SELECT * FROM "shelving_units" WHERE user_id = $1 AND "shelving_units"."deleted_at" IS NULL AND "shelving_units"."id" = $2 ORDER BY "shelving_units"."id" LIMIT 1`)).
		WithArgs(userName, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "notes", "created_at", "updated_at", "user_id", "parent_id", "parent_category"}).
			AddRow(1, "Shelving Unit 1", "test notes", time.Now(), time.Now(), userName, 1, "room"))
}

func expectRoom(mockDB *sqlmock.Sqlmock, userName string) {
	(*mockDB).ExpectQuery(regexp.QuoteMeta(`SELECT * FROM "rooms" WHERE user_id = $1 AND "rooms"."deleted_at" IS NULL AND "rooms"."id" = $2 ORDER BY "rooms"."id" LIMIT 1`)).
		WithArgs(userName, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "notes", "created_at", "updated_at", "user_id", "parent_id", "parent_category"}).
			AddRow(1, "Room 1", "test notes", time.Now(), time.Now(), userName, 1, "building"))
}

func expectBuilding(mockDB *sqlmock.Sqlmock, userName string) {
	(*mockDB).ExpectQuery(regexp.QuoteMeta(`SELECT * FROM "buildings" WHERE user_id = $1 AND "buildings"."deleted_at" IS NULL AND "buildings"."id" = $2 ORDER BY "buildings"."id" LIMIT 1`)).
		WithArgs(userName, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "address", "notes", "created_at", "updated_at", "user_id"}).
			AddRow(1, "Building 1", "123 address", "test notes", time.Now(), time.Now(), userName))
}

func validateGetEntitiesSuccessResponse(t *testing.T, res *http.Response, mockDB sqlmock.Sqlmock, mockCache redismock.ClientMock) {
	if res.StatusCode != http.StatusOK {
		t.Errorf("Expected status code to be: %d. Got: %d.", http.StatusOK, res.StatusCode)
	}

	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Errorf("Expected error to be nil. Got: %v", err)
	}

	contents := getEntitiesSingleResponse{}
	err = json.Unmarshal(data, &contents)
	if err != nil {
		t.Errorf("Expected error to be nil. Got: %v", err)
	}

	if contents.Message != "success" {
		t.Errorf("Expected message to be 'success'. Got: %s", contents.Message)
	}

	dataType := reflect.TypeOf(contents.Data).String()
	if dataType != "models.GetEntitiesResponse" {
		t.Errorf("Expected data to be type models.GetEntitiesResponse. Got: %v", dataType)
	}

	dataType = reflect.TypeOf(contents.Data.TotalCount).String()
	if dataType != "int" {
		t.Errorf("Expected TotalCount to be type int. Got: %v", dataType)
	}

	dataType = reflect.TypeOf(contents.Data.Entities).String()
	if dataType != "[]models.GetEntitiesEntity" {
		t.Errorf("Expected TotalCount to be type []models.GetEntitiesResponseData. Got: %v", dataType)
	}

	if err := mockDB.ExpectationsWereMet(); err != nil {
		t.Errorf("PostGres expectations were not met: %v", err)
	}

	if err := mockCache.ExpectationsWereMet(); err != nil {
		t.Errorf("Redis expectations were not met: %v", err)
	}
}

// TestCreateEntity runs the unit tests for invalid cases.
func TestGetEntities(t *testing.T) {
	cases := []getEntitiesTestCase{
		{
			testName:       "BEUT-27: Get Entities Valid URL Param Cache Miss",
			testUser:       "testuser0",
			testCacheHit:   false,
			testValidInput: true,
			offset:         "25",
			limit:          "25",
		},
		{
			testName:       "BEUT-28: Get Entities Valid URL Param Cache Hit",
			testUser:       "testuser0",
			testCacheHit:   true,
			testValidInput: true,
			offset:         "60",
			limit:          "30",
		},
		{
			testName:       "BEUT-29: Get Entities No Offset Param Cache Miss",
			testUser:       "testuser1",
			testCacheHit:   false,
			testValidInput: true,
			limit:          "15",
		},
		{
			testName:       "BEUT-30: Get Entities No Offset Param Cache Hit",
			testUser:       "testuser1",
			testCacheHit:   true,
			testValidInput: true,
			limit:          "60",
		},
		{
			testName:       "BEUT-31: Get Entities No Limit Param Cache Miss",
			testUser:       "testuser2",
			testCacheHit:   false,
			testValidInput: true,
			offset:         "15",
		},
		{
			testName:       "BEUT-32: Get Entities No Limit Param Cache Hit",
			testUser:       "testuser2",
			testCacheHit:   true,
			testValidInput: true,
			offset:         "20",
		},
		{
			testName:       "BEUT-33: Get Entities No Params Cache Miss",
			testUser:       "testuser3",
			testCacheHit:   false,
			testValidInput: true,
		},
		{
			testName:       "BEUT-34: Get Entities No Params Cache Hit",
			testUser:       "testuser3",
			testCacheHit:   true,
			testValidInput: true,
		},
		{
			testName:       "BEUT-35: Get Entities Invalid Offset Non-Integer",
			testUser:       "testuser4",
			testValidInput: false,
			offset:         "not int",
		},
		{
			testName:       "BEUT-36: Get Entities Invalid Limit Non-Integer",
			testUser:       "testuser5",
			testValidInput: false,
			limit:          "not int",
		},
		{
			testName:       "BEUT-37: Get Entities Invalid Params Non-Integer",
			testUser:       "testuser6",
			testValidInput: false,
			offset:         "not int",
			limit:          "not int also",
		},
		{
			testName:       "BEUT-38: Get Entities Invalid Offset Negative Integer",
			testUser:       "testuser7",
			testValidInput: false,
			offset:         "-15",
		},
		{
			testName:       "BEUT-39: Get Entities Invalid Limit Negative Integer",
			testUser:       "testuser8",
			testValidInput: false,
			limit:          "-20",
		},
		{
			testName:       "BEUT-40: Get Entities Invalid Params Negative Integer",
			testUser:       "testuser9",
			testValidInput: false,
			offset:         "-10",
			limit:          "-20",
		},
	}

	for _, tc := range cases {
		client, srv, mockDB, mockCache := setupGetEntitiesTest(t, tc.testUser)
		t.Run(tc.testName, func(t *testing.T) {

			if tc.testCacheHit {
				setupGetEntitiesCacheHitMockExpectations(mockCache, tc.testUser, tc.offset, tc.limit)
			} else if !tc.testCacheHit {
				setupGetEntitiesCacheMissMockExpectations(&mockDB, mockCache, tc.testUser, tc.offset, tc.limit)
			}

			res, err := client.Get(fmt.Sprintf("%s%s?offset=%s&limit=%s", srv.URL, getEntitiesEndpoint, tc.offset, tc.limit))
			if err != nil {
				t.Fatalf("Failed to send request: %v", err)
			}
			defer res.Body.Close()

			if tc.testValidInput {
				validateGetEntitiesSuccessResponse(t, res, mockDB, mockCache)
			} else if (!tc.testValidInput) && (res.StatusCode != http.StatusBadRequest) {
				t.Errorf("Expected status code to be: %d. Got: %d.", http.StatusBadRequest, res.StatusCode)
			}

		})
	}
}
