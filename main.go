// Copyright © 2026 Valentin Slyusarev <va.slyusarev@gmail.com>
package main

import (
    "context"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
)

//go:embed web/template.html
var htmlTemplate string

// Версия приложения
const version = "dev"

// Структуры для API
type FileInfo struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
	Modified  string `json:"modified"`
	IsDir     bool   `json:"isDir"`
	Directory string `json:"directory"`
	FullPath  string `json:"fullPath"`
}

type FileListResponse struct {
	Path  string     `json:"path"`
	Files []FileInfo `json:"files"`
	Error string     `json:"error,omitempty"`
}

type HealthResponse struct {
	Status      string `json:"status"`
	Version     string `json:"version"`
	Path        string `json:"path"`
	PreviewSize int64  `json:"previewSize"`
	ServerPort  string `json:"serverPort"`
	Encoding    string `json:"encoding"`
}

// Конфигурация сервера
type ServerConfig struct {
	LogDirectory string
	PreviewSize  int64
	ServerPort   string
	Encoding     string
}

// Глобальная конфигурация
var config ServerConfig

// Map для кодировок
var encodings map[string]encoding.Encoding

func init() {
	// Инициализируем доступные кодировки
	encodings = map[string]encoding.Encoding{
		"utf-8":      nil, // nil означает, что преобразование не нужно
		"windows-1251": charmap.Windows1251,
		"cp1251":     charmap.Windows1251,
		"win1251":    charmap.Windows1251,
		"koi8-r":     charmap.KOI8R,
		"iso-8859-1": charmap.ISO8859_1,
		"cp866":      charmap.CodePage866,
	}
}

func main() {
	// Парсинг флагов командной строки
	defaultLogDir := getDefaultLogDir()

	flag.StringVar(&config.LogDirectory, "dir", defaultLogDir,
		"Каталог с лог-файлами (по умолчанию: ./logs относительно исполняемого файла)")

	flag.Int64Var(&config.PreviewSize, "preview-limit", 1*1024*1024,
		"Максимальный размер файла для предпросмотра в БАЙТАХ (по умолчанию: 1 МБ = 1_048_576 байт)")

	flag.StringVar(&config.ServerPort, "port", "7424",
		"Порт для запуска сервера (по умолчанию: 7424)")

	flag.StringVar(&config.Encoding, "encoding", "utf-8",
		"Кодировка лог-файлов: utf-8, win1251/cp1251, koi8-r, iso-8859-1, cp866")

	flag.Parse()

	// Проверяем поддержку кодировки
	if _, ok := encodings[config.Encoding]; !ok {
		supported := make([]string, 0, len(encodings))
		for k := range encodings {
			supported = append(supported, k)
		}
		log.Fatalf("❌ Неподдерживаемая кодировка: %s. Доступные: %s",
			config.Encoding, strings.Join(supported, ", "))
	}

	log.Printf("🚀 Web-просмотрщик логов (версия: %s)", version)
	log.Printf("📁 Каталог логов: %s", config.LogDirectory)
	log.Printf("📏 Макс. размер для предпросмотра: %d байт (%.2f МБ)",
	    config.PreviewSize, float64(config.PreviewSize)/(1024*1024))
	log.Printf("🔤 Кодировка файлов: %s", config.Encoding)
	log.Printf("🌐 Порт сервера: %s", config.ServerPort)

	// Проверяем существование каталога логов
	if _, err := os.Stat(config.LogDirectory); os.IsNotExist(err) {
		log.Printf("⚠️  Каталог логов не существует: %s", config.LogDirectory)
		log.Printf("ℹ️  Создаю каталог...")
		if err := os.MkdirAll(config.LogDirectory, 0755); err != nil {
			log.Fatalf("❌ Не удалось создать каталог логов: %v", err)
		}
		log.Printf("✅ Каталог создан: %s", config.LogDirectory)
	}

	// Настройка роутинга
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/index.html", handleIndex)
	mux.HandleFunc("/api/health", handleHealth)
	mux.HandleFunc("/api/files", handleFileList)
	mux.HandleFunc("/api/file", handleFileContent)
	mux.HandleFunc("/api/download", handleFileDownload)
	mux.HandleFunc("/api/config", handleConfig)

	// Настройка сервера с таймаутами
	server := &http.Server{
		Addr:         ":" + config.ServerPort,
		Handler:      corsMiddleware(mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	// Канал для graceful shutdown
	serverClosed := make(chan struct{})

	// Обработка сигналов остановки
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan

		log.Printf("🛑 Получен сигнал остановки...")
		log.Printf("⏳ Завершение работы сервера...")

		// Создаем контекст с таймаутом для graceful shutdown
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			log.Printf("❌ Ошибка при graceful shutdown: %v", err)
		}

		log.Printf("✅ Сервер успешно остановлен")
		close(serverClosed)
	}()

	log.Printf("🌐 Сервер запущен: http://localhost:%s", config.ServerPort)
	log.Printf("📱 Веб-интерфейс доступен по корневому URL")
	log.Printf("🛑 Для остановки сервера нажмите Ctrl+C")

	// Запуск сервера в отдельной горутине для graceful shutdown
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("❌ Ошибка запуска сервера: %v", err)
		}
	}()

	// Ожидание сигнала остановки
	<-serverClosed
	log.Printf("👋 До свидания!")
}

// CORS middleware
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept, Content-Encoding")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Получение каталога по умолчанию
func getDefaultLogDir() string {
	exePath, err := os.Executable()
	if err != nil {
		currentDir, _ := os.Getwd()
		return filepath.Join(currentDir, "logs")
	}

	exeDir := filepath.Dir(exePath)
	return filepath.Join(exeDir, "logs")
}

// Структура для передачи данных в шаблон
type TemplateData struct {
	LogDirectory string
	PreviewSize  int64
	PreviewSizeMB string // Для отображения на фронте в МБ
	ServerPort   string
	Version      string
	Encoding     string
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	data := TemplateData{
		LogDirectory: config.LogDirectory,
		PreviewSize:  config.PreviewSize,
		PreviewSizeMB: fmt.Sprintf("%.2f", float64(config.PreviewSize)/(1024*1024)),
		ServerPort:   config.ServerPort,
		Version:      version,
		Encoding:     config.Encoding,
	}

	tmpl, err := template.New("index").Parse(htmlTemplate)
	if err != nil {
		http.Error(w, "Ошибка создания шаблона", http.StatusInternalServerError)
		log.Printf("❌ Ошибка парсинга шаблона: %v", err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Всегда используем сжатие для HTML
	w.Header().Set("Content-Encoding", "gzip")
	gz := gzip.NewWriter(w)
	defer gz.Close()

	if err := tmpl.Execute(gz, data); err != nil {
		log.Printf("❌ Ошибка выполнения шаблона: %v", err)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	response := HealthResponse{
		Status:      "ok",
		Version:     version,
		Path:        config.LogDirectory,
		PreviewSize: config.PreviewSize,
		ServerPort:  config.ServerPort,
		Encoding:    config.Encoding,
	}

	sendJSONResponse(w, r, response)
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"logDirectory": config.LogDirectory,
		"previewSize":  config.PreviewSize,
		"serverPort":   config.ServerPort,
		"version":      version,
		"encoding":     config.Encoding,
		"startTime":    time.Now().Format(time.RFC3339),
	}

	sendJSONResponse(w, r, response)
}

func handleFileList(w http.ResponseWriter, r *http.Request) {
	response := FileListResponse{
		Path: config.LogDirectory,
	}

	// Проверяем существование директории
	if _, err := os.Stat(config.LogDirectory); os.IsNotExist(err) {
		response.Error = fmt.Sprintf("Каталог не существует: %s", config.LogDirectory)
		w.WriteHeader(http.StatusNotFound)
		sendJSONResponse(w, r, response)
		return
	}

	// Читаем файлы рекурсивно
	err := filepath.Walk(config.LogDirectory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if path == config.LogDirectory {
			return nil
		}

		// Если это директория, не добавляем в список файлов
		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(config.LogDirectory, path)
		if err != nil {
			return err
		}

		dir := filepath.Dir(relPath)
		if dir == "." {
			dir = ""
		}

		fileInfo := FileInfo{
			Name:      info.Name(),
			SizeBytes: info.Size(),
			Modified:  info.ModTime().Format(time.RFC3339),
			IsDir:     info.IsDir(),
			Directory: dir,
			FullPath:  relPath,
		}

		response.Files = append(response.Files, fileInfo)
		return nil
	})

	if err != nil {
		response.Error = fmt.Sprintf("Ошибка чтения директории: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
	}

	sendJSONResponse(w, r, response)
}

func handleFileContent(w http.ResponseWriter, r *http.Request) {
	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		http.Error(w, "Не указан путь к файлу", http.StatusBadRequest)
		return
	}

	// Безопасная проверка пути
	fullPath := filepath.Join(config.LogDirectory, filePath)

	// Проверяем, что путь находится внутри LogDirectory
	cleanPath, err := filepath.Abs(fullPath)
	if err != nil {
		http.Error(w, "Некорректный путь", http.StatusBadRequest)
		return
	}

	cleanDir, err := filepath.Abs(config.LogDirectory)
	if err != nil {
		http.Error(w, "Ошибка сервера", http.StatusInternalServerError)
		return
	}

	if !strings.HasPrefix(cleanPath, cleanDir) {
		http.Error(w, "Доступ запрещен", http.StatusForbidden)
		return
	}

	// Проверяем существование файла
	info, err := os.Stat(fullPath)
	if err != nil {
		http.Error(w, "Файл не найден", http.StatusNotFound)
		return
	}

	// Открываем файл
	file, err := os.Open(fullPath)
	if err != nil {
		http.Error(w, "Ошибка открытия файла", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	// Подготовка сообщения о предпросмотре (в UTF-8)
	var message []byte
	var fileContent []byte

	if info.Size() > config.PreviewSize {
		// Формируем сообщение о предпросмотре в UTF-8
		message = []byte(fmt.Sprintf("⚠️ Файл слишком большой (%.2f МБ). Показаны последние %d байт (%.2f МБ). Полный файл доступен для скачивания.\n",
			float64(info.Size())/(1024*1024), config.PreviewSize, float64(config.PreviewSize)/(1024*1024)))

		// Читаем только последние config.PreviewSize байт из файла
		offset := info.Size() - config.PreviewSize
		if offset < 0 {
			offset = 0
		}

		// Перемещаем указатель
		_, err = file.Seek(offset, io.SeekStart)
		if err != nil {
			http.Error(w, "Ошибка чтения файла", http.StatusInternalServerError)
			return
		}

		// Читаем данные
		fileContent = make([]byte, config.PreviewSize)
		n, err := io.ReadFull(file, fileContent)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			http.Error(w, "Ошибка чтения файла", http.StatusInternalServerError)
			return
		}
		fileContent = fileContent[:n]
	} else {
		// Читаем весь файл
		fileContent, err = io.ReadAll(file)
		if err != nil {
			http.Error(w, "Ошибка чтения файла", http.StatusInternalServerError)
			return
		}
	}

	// Сначала конвертируем содержимое файла в UTF-8 если нужно
	var convertedContent []byte
	if config.Encoding != "utf-8" && encodings[config.Encoding] != nil {
		decoder := encodings[config.Encoding].NewDecoder()
		converted, err := decoder.Bytes(fileContent)
		if err != nil {
			// Если не удалось конвертировать, оставляем как есть
			log.Printf("⚠️ Не удалось конвертировать файл %s из %s в UTF-8: %v", filepath.Base(filePath), config.Encoding, err)
			convertedContent = fileContent
		} else {
			convertedContent = converted
		}
	} else {
		convertedContent = fileContent
	}

	// Объединяем сообщение (уже в UTF-8) с конвертированным содержимым файла
	var finalContent []byte
	if info.Size() > config.PreviewSize {
		finalContent = append(message, convertedContent...)
	} else {
		finalContent = convertedContent
	}

	// Устанавливаем заголовки и отправляем
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	sendCompressed(w, r, finalContent)
}

func handleFileDownload(w http.ResponseWriter, r *http.Request) {
	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		http.Error(w, "Не указан путь к файлу", http.StatusBadRequest)
		return
	}

	// Безопасная проверка пути (используем ту же функцию что и выше)
	fullPath := filepath.Join(config.LogDirectory, filePath)

	cleanPath, err := filepath.Abs(fullPath)
	if err != nil {
		http.Error(w, "Некорректный путь", http.StatusBadRequest)
		return
	}

	cleanDir, err := filepath.Abs(config.LogDirectory)
	if err != nil {
		http.Error(w, "Ошибка сервера", http.StatusInternalServerError)
		return
	}

	if !strings.HasPrefix(cleanPath, cleanDir) {
		http.Error(w, "Доступ запрещен", http.StatusForbidden)
		return
	}

	// Проверяем существование файла
	info, err := os.Stat(fullPath)
	if err != nil {
		http.Error(w, "Файл не найден", http.StatusNotFound)
		return
	}

	// Открываем файл
	file, err := os.Open(fullPath)
	if err != nil {
		http.Error(w, "Ошибка открытия файла", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	// Устанавливаем заголовки для скачивания
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", info.Name()))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))

	// Копируем файл в response
	io.Copy(w, file)
}

// Отправка JSON с сжатием
func sendJSONResponse(w http.ResponseWriter, r *http.Request, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	jsonData, err := json.Marshal(data)
	if err != nil {
		http.Error(w, "Ошибка сериализации JSON", http.StatusInternalServerError)
		return
	}

	sendCompressed(w, r, jsonData)
}

// Универсальная функция отправки сжатых данных (всегда сжимаем)
func sendCompressed(w http.ResponseWriter, r *http.Request, data []byte) {
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Vary", "Accept-Encoding")

	gz := gzip.NewWriter(w)
	defer gz.Close()

	if _, err := gz.Write(data); err != nil {
		log.Printf("❌ Ошибка записи сжатых данных: %v", err)
	}
}