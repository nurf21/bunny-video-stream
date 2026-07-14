package main

import (
	"bytes"
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
	godotenv.Load(".env")
	BunnyAPIKey = os.Getenv("BUNNY_API_KEY")
	BunnyLibraryID = os.Getenv("BUNNY_LIBRARY_ID")
	BunnyPullDomain = os.Getenv("BUNNY_PULL_ZONE_DOMAIN")
	DBConnStr = os.Getenv("DB_CONN_STR")

	if BunnyAPIKey == "" || BunnyLibraryID == "" || DBConnStr == "" {
		log.Fatal("Environment variables tidak lengkap!")
	}

	var err error
	db, err = sql.Open("postgres", DBConnStr)
	if err != nil {
		log.Fatal("Gagal koneksi Database: ", err.Error())
	}
}

func main() {
	initInfra()

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
	title := c.FormValue("title")
	creator := c.FormValue("creator")
	description := c.FormValue("description")

	thumbnailURL := ""

	videoFile, err := c.FormFile("video")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Video wajib diupload"})
	}
	videoSrc, _ := videoFile.Open()
	defer videoSrc.Close()

	bunnyVideoID, err := createBunnyVideoPlaceholder(title)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Bunny API Error (Create): " + err.Error()})
	}

	err = uploadVideoToBunny(bunnyVideoID, videoSrc)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Bunny API Error (Upload): " + err.Error()})
	}

	videoURL := fmt.Sprintf("https://%s/%s/playlist.m3u8", BunnyPullDomain, bunnyVideoID)

	query := `INSERT INTO courses (title, thumbnail_url, creator, description, video_url, video_id) VALUES ($1, $2, $3, $4, $5, $6)`
	_, err = db.Exec(query, title, thumbnailURL, creator, description, videoURL, bunnyVideoID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Gagal simpan ke DB: " + err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{"message": "Course berhasil dibuat dan video sedang diproses secara cloud di Bunny Stream!"})
}

// === [READ ALL] ===
func handleGetAllCourses(c echo.Context) error {
	rows, err := db.Query("SELECT id, title, thumbnail_url, creator, description, video_url, video_id FROM courses ORDER BY id DESC")
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	defer rows.Close()

	var courses []Course
	for rows.Next() {
		var crs Course
		if err := rows.Scan(&crs.ID, &crs.Title, &crs.ThumbnailURL, &crs.Creator, &crs.Description, &crs.VideoURL, &crs.VideoID); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}

		crs.VideoURL = GenerateSecureVideoURL(crs.VideoID)
		fmt.Printf("crs.VideoURL: %v\n", crs.VideoURL)
		courses = append(courses, crs)
	}

	if err := rows.Err(); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, courses)
}

// === [UPDATE COURSE INFO] ===
func handleUpdateCourse(c echo.Context) error {
	id := c.Param("id")
	title := c.FormValue("title")
	creator := c.FormValue("creator")
	description := c.FormValue("description")

	query := `UPDATE courses SET title=$1, creator=$2, description=$3 WHERE id=$4`
	_, err := db.Exec(query, title, creator, description, id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]string{"message": "Info Course berhasil diperbarui!"})
}

// === [DELETE COURSE + VIDEO DI BUNNY STREAM] ===
func handleDeleteCourse(c echo.Context) error {
	id := c.Param("id")

	var bunnyVideoID string
	err := db.QueryRow("SELECT video_id FROM courses WHERE id = $1", id).Scan(&bunnyVideoID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "Course tidak ditemukan"})
	}

	err = deleteVideoFromBunny(bunnyVideoID)
	if err != nil {
		log.Printf("[WARNING] Gagal menghapus video di Bunny Stream: %v", err)
	}

	_, err = db.Exec("DELETE FROM courses WHERE id = $1", id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{"message": "Course dan file video di Bunny Stream sukses dihapus permanen!"})
}

func createBunnyVideoPlaceholder(title string) (string, error) {
	url := fmt.Sprintf("https://video.bunnycdn.com/library/%s/videos", BunnyLibraryID)
	payload, _ := json.Marshal(map[string]string{"title": title})

	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(payload))
	req.Header.Set("AccessKey", BunnyAPIKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	if id, ok := result["guid"].(string); ok {
		return id, nil
	}
	return "", fmt.Errorf("gagal mendapatkan guid dari Bunny Stream")
}

func uploadVideoToBunny(videoID string, videoData io.Reader) error {
	url := fmt.Sprintf("https://video.bunnycdn.com/library/%s/videos/%s", BunnyLibraryID, videoID)

	req, _ := http.NewRequest("PUT", url, videoData)
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

func deleteVideoFromBunny(videoID string) error {
	url := fmt.Sprintf("https://video.bunnycdn.com/library/%s/videos/%s", BunnyLibraryID, videoID)

	req, _ := http.NewRequest("DELETE", url, nil)
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
	tokenKey := os.Getenv("BUNNY_TOKEN_KEY")
	baseUrl := "https://iframe.mediadelivery.net/embed"

	expiration := time.Now().Unix() + 1800

	hashable := fmt.Sprintf("%s%s%d", tokenKey, videoID, expiration)

	hasher := sha256.New()
	hasher.Write([]byte(hashable))
	shaHash := hasher.Sum(nil)

	token := hex.EncodeToString(shaHash)

	secureURL := fmt.Sprintf("%s/%s/%s?token=%s&expires=%d",
		baseUrl,
		BunnyLibraryID,
		videoID,
		token,
		expiration,
	)

	return secureURL
}
