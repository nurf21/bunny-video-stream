package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	_ "github.com/lib/pq"
)

var (
	BunnyAPIKey     string
	BunnyLibraryID  string
	BunnyPullDomain string
	BunnyTokenKey   string
	DBConnStr       string
	db              *sql.DB
)

type Course struct {
	ID           int    `json:"id"`
	Title        string `json:"title"`
	ThumbnailURL string `json:"thumbnail_url"`
	Creator      string `json:"creator"`
	Description  string `json:"description"`
	VideoURL     string `json:"video_url"`
	VideoID      string `json:"video_id"`
}

func initInfra() {
	_ = godotenv.Load(".env")

	BunnyAPIKey = os.Getenv("BUNNY_API_KEY")
	BunnyLibraryID = os.Getenv("BUNNY_LIBRARY_ID")
	BunnyPullDomain = os.Getenv("BUNNY_PULL_ZONE_DOMAIN")
	BunnyTokenKey = os.Getenv("BUNNY_TOKEN_KEY")
	DBConnStr = os.Getenv("DB_CONN_STR")

	if BunnyAPIKey == "" || BunnyLibraryID == "" || DBConnStr == "" || BunnyTokenKey == "" {
		log.Fatal("Environment variables tidak lengkap!")
	}

	var err error
	db, err = sql.Open("postgres", DBConnStr)
	if err != nil {
		log.Fatal("Gagal koneksi Database: ", err.Error())
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		log.Fatal("Database tidak merespon PING: ", err.Error())
	}
}

func main() {
	initInfra()
	defer db.Close()

	e := echo.New()
	e.Use(middleware.RequestLogger())
	e.Use(middleware.Recover())
	e.Use(middleware.CORS())

	e.POST("/courses", handleCreateCourse)
	e.GET("/courses", handleGetAllCourses)
	e.PUT("/courses/:id", handleUpdateCourse)
	e.DELETE("/courses/:id", handleDeleteCourse)

	e.Logger.Fatal(e.Start(":5000"))
}

// === [CREATE COURSE] ===
func handleCreateCourse(c echo.Context) error {
	ctx := c.Request().Context()

	title := c.FormValue("title")
	creator := c.FormValue("creator")
	description := c.FormValue("description")

	if title == "" || creator == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Title dan Creator wajib diisi"})
	}

	videoFile, err := c.FormFile("video")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Video wajib diupload"})
	}

	videoSrc, err := videoFile.Open()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Gagal membuka file video"})
	}
	defer videoSrc.Close()

	bunnyVideoID, err := createBunnyVideoPlaceholder(ctx, title)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Bunny API Error (Create): " + err.Error()})
	}

	err = uploadVideoToBunny(ctx, bunnyVideoID, videoSrc)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Bunny API Error (Upload): " + err.Error()})
	}

	videoURL := fmt.Sprintf("https://%s/%s/playlist.m3u8", BunnyPullDomain, bunnyVideoID)
	thumbnailURL := ""

	query := `INSERT INTO courses (title, thumbnail_url, creator, description, video_url, video_id) VALUES ($1, $2, $3, $4, $5, $6)`
	_, err = db.ExecContext(ctx, query, title, thumbnailURL, creator, description, videoURL, bunnyVideoID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Gagal simpan ke DB: " + err.Error()})
	}

	return c.JSON(http.StatusCreated, map[string]string{"message": "Course berhasil dibuat dan video sedang diproses secara cloud di Bunny Stream!"})
}

// === [READ ALL] ===
func handleGetAllCourses(c echo.Context) error {
	ctx := c.Request().Context()

	rows, err := db.QueryContext(ctx, "SELECT id, title, thumbnail_url, creator, description, video_url, video_id FROM courses ORDER BY id DESC")
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	defer rows.Close()

	courses := make([]Course, 0)
	for rows.Next() {
		var crs Course
		if err := rows.Scan(&crs.ID, &crs.Title, &crs.ThumbnailURL, &crs.Creator, &crs.Description, &crs.VideoURL, &crs.VideoID); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}

		crs.VideoURL = GenerateSecureVideoURL(crs.VideoID)
		courses = append(courses, crs)
	}

	if err := rows.Err(); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, courses)
}

// === [UPDATE COURSE INFO] ===
func handleUpdateCourse(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")
	title := c.FormValue("title")
	creator := c.FormValue("creator")
	description := c.FormValue("description")

	query := `UPDATE courses SET title=$1, creator=$2, description=$3 WHERE id=$4`
	res, err := db.ExecContext(ctx, query, title, creator, description, id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "Course tidak ditemukan"})
	}

	return c.JSON(http.StatusOK, map[string]string{"message": "Info Course berhasil diperbarui!"})
}

// === [DELETE COURSE + VIDEO DI BUNNY STREAM] ===
func handleDeleteCourse(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")

	var bunnyVideoID string
	err := db.QueryRowContext(ctx, "SELECT video_id FROM courses WHERE id = $1", id).Scan(&bunnyVideoID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "Course tidak ditemukan"})
	}

	err = deleteVideoFromBunny(ctx, bunnyVideoID)
	if err != nil {
		log.Printf("[WARNING] Gagal menghapus video di Bunny Stream: %v", err)
	}

	_, err = db.ExecContext(ctx, "DELETE FROM courses WHERE id = $1", id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{"message": "Course dan file video di Bunny Stream sukses dihapus permanen!"})
}

func createBunnyVideoPlaceholder(ctx context.Context, title string) (string, error) {
	url := fmt.Sprintf("https://video.bunnycdn.com/library/%s/videos", BunnyLibraryID)
	payload, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("AccessKey", BunnyAPIKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bunny create mengembalikan status: %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if id, ok := result["guid"].(string); ok {
		return id, nil
	}
	return "", fmt.Errorf("gagal mendapatkan guid dari Bunny Stream")
}

func uploadVideoToBunny(ctx context.Context, videoID string, videoData io.Reader) error {
	url := fmt.Sprintf("https://video.bunnycdn.com/library/%s/videos/%s", BunnyLibraryID, videoID)

	req, err := http.NewRequestWithContext(ctx, "PUT", url, videoData)
	if err != nil {
		return err
	}
	req.Header.Set("AccessKey", BunnyAPIKey)
	req.Header.Set("Content-Type", "application/octet-stream")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bunny upload mengembalikan status code: %d", resp.StatusCode)
	}
	return nil
}

func deleteVideoFromBunny(ctx context.Context, videoID string) error {
	url := fmt.Sprintf("https://video.bunnycdn.com/library/%s/videos/%s", BunnyLibraryID, videoID)

	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("AccessKey", BunnyAPIKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bunny delete mengembalikan status: %d", resp.StatusCode)
	}
	return nil
}

func GenerateSecureVideoURL(videoID string) string {
	baseUrl := "https://iframe.mediadelivery.net/embed"
	expiration := time.Now().Unix() + 1800

	hashable := fmt.Sprintf("%s%s%d", BunnyTokenKey, videoID, expiration)

	hasher := sha256.New()
	hasher.Write([]byte(hashable))
	token := hex.EncodeToString(hasher.Sum(nil))

	return fmt.Sprintf("%s/%s/%s?token=%s&expires=%d",
		baseUrl,
		BunnyLibraryID,
		videoID,
		token,
		expiration,
	)
}
