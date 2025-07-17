package api

import (
	"context"
	"net/http"
	"observer_service/internal/models"
	"observer_service/internal/processor"
	"observer_service/internal/services/storage"
	"time"

	"github.com/gin-gonic/gin"
)

type Server struct {
	router    *gin.Engine
	processor *processor.LogProcessor
	storage   storage.IPStorage
	port      string
}

func NewServer(port string, proc *processor.LogProcessor, storage storage.IPStorage) *Server {
	gin.SetMode(gin.ReleaseMode)
	router := gin.Default()
	router.Use(gin.Logger())
	router.Use(gin.Recovery())

	s := &Server{
		router:    router,
		processor: proc,
		storage:   storage,
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

	if err := s.storage.Ping(ctx); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status":           "error",
			"redis_connection": "failed",
			"error":            err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":           "ok",
		"redis_connection": "ok",
	})
}