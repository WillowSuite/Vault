package controllers

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"willowsuite-vault/config"
	"willowsuite-vault/helpers"
	"willowsuite-vault/infra/cache"
	"willowsuite-vault/models"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/yeqown/go-qrcode/v2"
	"github.com/yeqown/go-qrcode/writer/standard"
)

type PresignedURLCacheKey struct {
	CacheKey cache.CacheKey
	Category string
	ID       string
}

// Generate creates a new QR code, uploads it to S3, and then returns the url to the frontend.
func (handler Handler) Generate(w http.ResponseWriter, request *http.Request) {
	//Get parameters
	byteData, err := io.ReadAll(request.Body)
	if err != nil {
		logAndRespond(w, "Error parsing request", err)
		return
	}

	var parsedData map[string]string
	if err = json.Unmarshal(byteData, &parsedData); err != nil {
		logAndRespond(w, "Error parsing json", err)
		return
	}

	//Validate parameters
	category, stringID := parsedData["category"], parsedData["id"]
	if category == "" {
		logAndRespond(w, "Missing category", nil)
		return
	}

	if stringID == "" {
		logAndRespond(w, "Missing id", nil)
		return
	}

	id, err := strconv.ParseUint(stringID, 10, 64)
	if err != nil {
		logAndRespond(w, fmt.Sprintf("ID must be type integer: %v", stringID), nil)
		return
	}

	claims := request.Context().Value("user_claims").(jwt.MapClaims)
	userID := claims["username"].(string)

	cacheTTL := 500 * time.Second

	keyStructured := PresignedURLCacheKey{
		CacheKey: cache.CacheKey{
			User:     userID,
			Function: "GenerateQR",
		},
		Category: category,
		ID:       stringID,
	}

	key, err := json.Marshal(keyStructured)
	if err != nil {
		logAndRespond(w, fmt.Sprintf("Error encoding Redis key: %v", err), err)
		return
	}

	value, redisErr := handler.Repository.Cache.Get(request.Context(), string(key)).Result()
	if redisErr != nil && !errors.Is(redisErr, redis.Nil) {
		logAndRespond(w, fmt.Sprintf("Error retriving entites from Redis: %v", redisErr), err)
		return
	}

	if value == "" {
		// Verify entity exists
		entity := models.Entity{
			ID: id,
		}

		validEntity, model := buildEntity(entity, models.Parent{}, category, "")
		if !validEntity {
			logAndRespond(w, fmt.Sprintf("Invalid category %v.", category), nil)
			return
		}

		dberr := handler.Repository.GetOne(model, userID)
		if dberr != nil {
			logAndRespond(w, fmt.Sprintf("Entity category of %v with id %v not found.", category, stringID), nil)
			return
		}

		// Check if object exists
		bucketName := config.S3BucketName()
		folderName, err := Encrypt(userID, config.EncryptionSecert())
		if err != nil {
			logAndRespond(w, fmt.Sprintf("error encrypting your classified text: %v", err), err)
			return
		}

		folderName = strings.Replace(folderName, "/", "-", -1)
		fileName := fmt.Sprintf("QR-%s-%s.jpg", category, stringID)
		objectKey := fmt.Sprintf("%s/%s", folderName, fileName)

		_, err = handler.S3Client.HeadObject(request.Context(), &s3.HeadObjectInput{
			Bucket: aws.String(bucketName),
			Key:    aws.String(objectKey),
		})

		if err != nil {
			var notFound *types.NotFound
			if !errors.As(err, &notFound) {
				logAndRespond(w, fmt.Sprintf("Could not upload file: %v", err), err)
				return
			}

			// Build QR
			url := fmt.Sprintf("%s/%s/%s", config.FrontEndURL(), category, stringID)
			fileLocation := "assets/" + fileName

			qrc, err := qrcode.New(url)
			if err != nil {
				logAndRespond(w, "could not generate QRCode: %v", err)
				return
			}

			writer, err := standard.New(fileLocation)
			if err != nil {
				logAndRespond(w, "standard.New failed: %v", err)
				return
			}

			// Save TMP file
			if err = qrc.Save(writer); err != nil {
				logAndRespond(w, "could not save image: %v", err)
				return
			}

			// Upload to S3
			file, err := os.Open(fileLocation)
			if err != nil {
				logAndRespond(w, "Couldn't open file: %v", err)
				return
			}

			defer file.Close()
			defer os.Remove(fileLocation)

			_, err = handler.S3Client.PutObject(request.Context(), &s3.PutObjectInput{
				Bucket: aws.String(bucketName),
				Key:    aws.String(objectKey),
				Body:   file,
			})
			if err != nil {
				logAndRespond(w, fmt.Sprintf("Couldn't upload file: %v\n", err), err)
				return
			}

			err = s3.NewObjectExistsWaiter(handler.S3Client).Wait(
				request.Context(), &s3.HeadObjectInput{Bucket: aws.String(bucketName), Key: aws.String(objectKey)}, time.Minute)
			if err != nil {
				logAndRespond(w, fmt.Sprintf("Failed attempt to wait for object %s to exist.\n", fileName), err)
				return
			}
		}

		presigned, err := handler.S3PresignClient.PresignGetObject(request.Context(), &s3.GetObjectInput{
			Bucket: aws.String(bucketName),
			Key:    aws.String(objectKey),
		}, func(opts *s3.PresignOptions) {
			opts.Expires = time.Duration(cacheTTL)
		})
		if err != nil {
			logAndRespond(w, fmt.Sprintf("Couldn't get a presigned request: %v", err), err)
			return
		}

		value = presigned.URL
		handler.Repository.Cache.Set(request.Context(), string(key), value, cacheTTL)
	}

	helpers.SuccessResponse(w, value)
	return
}

// Encrypt method is to encrypt or hide any classified text
func Encrypt(text, MySecret string) (string, error) {
	var bytes = []byte{35, 46, 57, 24, 85, 35, 24, 74, 87, 35, 88, 98, 66, 32, 14, 05}
	block, err := aes.NewCipher([]byte(MySecret))
	if err != nil {
		return "", err
	}
	plainText := []byte(text)
	cfb := cipher.NewCFBEncrypter(block, bytes)
	cipherText := make([]byte, len(plainText))
	cfb.XORKeyStream(cipherText, plainText)
	return base64.StdEncoding.EncodeToString(cipherText), nil
}
