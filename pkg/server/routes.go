package server

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/images", s.handleListImages)
	s.mux.HandleFunc("POST /api/v1/images", s.handleCreateImage)
	s.mux.HandleFunc("GET /api/v1/images/{id}", s.handleGetImage)
	s.mux.HandleFunc("DELETE /api/v1/images/{id}", s.handleDeleteImage)
	s.mux.HandleFunc("GET /api/v1/nodes", s.handleListNodes)
	s.mux.HandleFunc("GET /api/v1/nodes/{id}/hardware", s.handleNodeHardware)
}
