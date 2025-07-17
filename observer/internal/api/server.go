package api

import (
	"context"
	"net/http"
	"observer_service/internal/models"
	"observer_service/internal/processor"
	"observer_service/internal/services/publisher"
	"observer_service/internal/services/storage"
	"time"

	"github.com/gin-gonic/gin"
)

type Server struct {
	router    *gin.Engine
	processor *processor.LogProcessor
	storage   storage.IPStorage
	publisher publisher.EventPublisher
	port      string
}

func NewServer(port string, proc *processor.LogProcessor, storage storage.IPStorage, pub publisher.EventPublisher) *Server {
	gin.SetMode(gin.ReleaseMode)
	router := gin.Default()
	router.Use(gin.Logger())
	router.Use(gin.Recovery())

	s := &Server{
		router:    router,
		processor: proc,
		storage:   storage,
		publisher: pub,
		port:      port,
	}

	s.setupRoutes()
	return s
}

func (s *Server) GetRouter() *gin.Engine {
	return s.router
}

func (s *Server) setupRoutes() {
	s.router.POST("/log-entry", s.handleProcessLogEntries)
	s.router.GET("/health", s.handleHealthCheck)
}

func (s *Server) Run() error {
	return s.router.Run(":" + s.port)
}

func (s *Server) handleProcessLogEntries(c *gin.Context) {
	var entries []models.LogEntry
	if err := c.ShouldBindJSON(&entries); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	s.processor.EnqueueEntries(entries)

	c.JSON(http.StatusAccepted, gin.H{
		"status":            "accepted",
		"processed_entries": len(entries),
	})
}

func (s *Server) handleHealthCheck(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status := http.StatusOK
	response := gin.H{
		"redis_connection":    "ok",
		"rabbitmq_connection": "ok",
	}

	// Check Redis
	if err := s.storage.Ping(ctx); err != nil {
		status = http.StatusServiceUnavailable
		response["redis_connection"] = "failed"
	}

	// Check RabbitMQ
	if err := s.publisher.Ping(); err != nil {
		status = http.StatusServiceUnavailable
		response["rabbitmq_connection"] = "failed"
	}

	if status == http.StatusOK {
		response["status"] = "ok"
	} else {
		response["status"] = "error"
	}

	c.JSON(status, response)
}