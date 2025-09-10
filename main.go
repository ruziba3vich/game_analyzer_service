package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/notnil/chess"
	"github.com/notnil/chess/uci"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ruziba3vich/game_analyzer_service/genprotos/genprotos/pgn_analyzer"
)

const (
	defaultPort          = ":8000"
	defaultAnalysisDepth = 15
	maxAnalysisDepth     = 25
	minMovesForAnalysis  = 10
	enginePoolSize       = 4
)

// GamePhase represents different phases of a chess game
type GamePhase int

const (
	Opening GamePhase = iota
	Middlegame
	Endgame
)

// EnginePool manages Stockfish engines for concurrent analysis
type EnginePool struct {
	engines chan *uci.Engine
	path    string
	size    int
}

func NewEnginePool(enginePath string, poolSize int) (*EnginePool, error) {
	pool := &EnginePool{
		engines: make(chan *uci.Engine, poolSize),
		path:    enginePath,
		size:    poolSize,
	}

	for i := 0; i < poolSize; i++ {
		engine, err := uci.New(enginePath)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("failed to create engine %d: %w", i, err)
		}

		if err := engine.Run(uci.CmdUCI, uci.CmdIsReady); err != nil {
			engine.Close()
			pool.Close()
			return nil, fmt.Errorf("failed to initialize engine %d: %w", i, err)
		}

		pool.engines <- engine
	}

	log.Printf("Initialized engine pool with %d Stockfish instances", poolSize)
	return pool, nil
}

func (p *EnginePool) Get() *uci.Engine {
	return <-p.engines
}

func (p *EnginePool) Put(engine *uci.Engine) {
	select {
	case p.engines <- engine:
	default:
		engine.Close()
		log.Println("Warning: Engine pool overflow, closing engine")
	}
}

func (p *EnginePool) Close() {
	close(p.engines)
	for engine := range p.engines {
		engine.Close()
	}
}

func (p *EnginePool) Size() int {
	return p.size
}

// ChessAnalysisServer implements the gRPC service
type ChessAnalysisServer struct {
	pb.UnimplementedChessAnalysisServiceServer
	enginePool *EnginePool
	startTime  time.Time
}

func NewChessAnalysisServer(enginePool *EnginePool) *ChessAnalysisServer {
	return &ChessAnalysisServer{
		enginePool: enginePool,
		startTime:  time.Now(),
	}
}

// parsePGN parses a PGN string and returns a chess game
func parsePGN(pgnStr string) (*chess.Game, error) {
	if strings.TrimSpace(pgnStr) == "" {
		return nil, fmt.Errorf("empty PGN string")
	}

	game := chess.NewGame()

	// Remove PGN headers (anything in square brackets)
	headerRegex := regexp.MustCompile(`\[.*?\]`)
	cleanPGN := headerRegex.ReplaceAllString(pgnStr, "")

	// Remove comments (anything in curly braces)
	commentRegex := regexp.MustCompile(`\{[^}]*\}`)
	cleanPGN = commentRegex.ReplaceAllString(cleanPGN, "")

	// Remove line breaks and normalize whitespace
	cleanPGN = strings.ReplaceAll(cleanPGN, "\n", " ")
	cleanPGN = strings.ReplaceAll(cleanPGN, "\r", " ")
	cleanPGN = strings.ReplaceAll(cleanPGN, "\t", " ")

	// Replace multiple spaces with single space
	spaceRegex := regexp.MustCompile(`\s+`)
	cleanPGN = spaceRegex.ReplaceAllString(cleanPGN, " ")
	cleanPGN = strings.TrimSpace(cleanPGN)

	// Split into tokens
	tokens := strings.Fields(cleanPGN)

	for _, token := range tokens {
		// Skip move numbers (e.g., "1.", "15.", etc.)
		moveNumberRegex := regexp.MustCompile(`^\d+\.+$`)
		if moveNumberRegex.MatchString(token) {
			continue
		}

		// Skip game results
		if token == "1-0" || token == "0-1" || token == "1/2-1/2" || token == "*" {
			break
		}

		// Clean annotations from the move (remove trailing !, ?, +, #)
		annotationRegex := regexp.MustCompile(`[!?+#]+$`)
		cleanMove := annotationRegex.ReplaceAllString(token, "")

		if cleanMove == "" {
			continue
		}

		// Try to decode the move using algebraic notation
		move, err := chess.AlgebraicNotation{}.Decode(game.Position(), cleanMove)
		if err != nil {
			// Try UCI notation as fallback
			move, err = chess.UCINotation{}.Decode(game.Position(), cleanMove)
			if err != nil {
				return nil, fmt.Errorf("failed to parse move '%s' at position %s: %v", cleanMove, game.Position().String(), err)
			}
		}

		// Apply the move to the game
		if err := game.Move(move); err != nil {
			return nil, fmt.Errorf("illegal move '%s' at position %s: %v", cleanMove, game.Position().String(), err)
		}
	}

	return game, nil
}

// AnalyzePGN implements the main analysis RPC
func (s *ChessAnalysisServer) AnalyzePGN(ctx context.Context, req *pb.AnalyzePGNRequest) (*pb.AnalyzePGNResponse, error) {
	// Validate request
	if req.Pgn == "" {
		return nil, status.Error(codes.InvalidArgument, "PGN cannot be empty")
	}

	depth := req.Depth
	if depth <= 0 {
		depth = defaultAnalysisDepth
	}
	if depth > maxAnalysisDepth {
		return nil, status.Errorf(codes.InvalidArgument, "depth cannot exceed %d", maxAnalysisDepth)
	}

	log.Printf("Starting PGN analysis (depth: %d)", depth)

	// Parse PGN using our custom parser
	game, err := parsePGN(req.Pgn)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid PGN: %v", err)
	}

	moves := game.Moves()
	if len(moves) < minMovesForAnalysis {
		return nil, status.Errorf(codes.InvalidArgument, "game too short for analysis (minimum %d moves required, got %d)", minMovesForAnalysis, len(moves))
	}

	// Perform analysis
	whiteElo, blackElo, err := s.analyzeGameMoves(ctx, game, int(depth))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "analysis failed: %v", err)
	}

	log.Printf("Analysis complete - White: O:%d M:%d E:%d | Black: O:%d M:%d E:%d",
		whiteElo.Opening, whiteElo.Middlegame, whiteElo.Endgame,
		blackElo.Opening, blackElo.Middlegame, blackElo.Endgame)

	return &pb.AnalyzePGNResponse{
		White: whiteElo,
		Black: blackElo,
	}, nil
}

// AnalyzePGNDetailed implements the detailed analysis RPC
func (s *ChessAnalysisServer) AnalyzePGNDetailed(ctx context.Context, req *pb.AnalyzePGNRequest) (*pb.DetailedAnalysisResponse, error) {
	// Validate request (same as AnalyzePGN)
	if req.Pgn == "" {
		return nil, status.Error(codes.InvalidArgument, "PGN cannot be empty")
	}

	depth := req.Depth
	if depth <= 0 {
		depth = defaultAnalysisDepth
	}
	if depth > maxAnalysisDepth {
		return nil, status.Errorf(codes.InvalidArgument, "depth cannot exceed %d", maxAnalysisDepth)
	}

	// Parse PGN using our custom parser
	game, err := parsePGN(req.Pgn)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid PGN: %v", err)
	}

	moves := game.Moves()
	if len(moves) < minMovesForAnalysis {
		return nil, status.Errorf(codes.InvalidArgument, "game too short for analysis")
	}

	// Perform detailed analysis
	whiteStats, blackStats, totalMoves, err := s.analyzeGameDetailed(ctx, game, int(depth))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "detailed analysis failed: %v", err)
	}

	return &pb.DetailedAnalysisResponse{
		White: &pb.PlayerEloAnalysis{
			Opening:    whiteStats.Opening.EstimatedElo,
			Middlegame: whiteStats.Middlegame.EstimatedElo,
			Endgame:    whiteStats.Endgame.EstimatedElo,
		},
		Black: &pb.PlayerEloAnalysis{
			Opening:    blackStats.Opening.EstimatedElo,
			Middlegame: blackStats.Middlegame.EstimatedElo,
			Endgame:    blackStats.Endgame.EstimatedElo,
		},
		WhiteStats:    whiteStats,
		BlackStats:    blackStats,
		TotalMoves:    int32(totalMoves),
		AnalysisDepth: int32(depth),
	}, nil
}

// HealthCheck implements the health check RPC
func (s *ChessAnalysisServer) HealthCheck(ctx context.Context, req *pb.HealthCheckRequest) (*pb.HealthCheckResponse, error) {
	return &pb.HealthCheckResponse{
		Status:         pb.HealthCheckResponse_SERVING,
		Message:        "Chess Analysis Service is healthy",
		Timestamp:      time.Now().Unix(),
		EnginePoolSize: int32(s.enginePool.Size()),
	}, nil
}

// Core analysis logic
type MoveEvaluation struct {
	MoveNumber    int
	Move          string
	Phase         GamePhase
	PlayerColor   chess.Color
	CentipawnLoss int
}

func (s *ChessAnalysisServer) analyzeGameMoves(ctx context.Context, game *chess.Game, depth int) (*pb.PlayerEloAnalysis, *pb.PlayerEloAnalysis, error) {
	evaluations, err := s.evaluateAllMoves(ctx, game, depth)
	if err != nil {
		return nil, nil, err
	}

	whiteElo := s.calculatePlayerElo(evaluations, chess.White)
	blackElo := s.calculatePlayerElo(evaluations, chess.Black)

	return whiteElo, blackElo, nil
}

func (s *ChessAnalysisServer) analyzeGameDetailed(ctx context.Context, game *chess.Game, depth int) (*pb.PhaseStatsByPhase, *pb.PhaseStatsByPhase, int, error) {
	moves := game.Moves()
	evaluations, err := s.evaluateAllMoves(ctx, game, depth)
	if err != nil {
		return nil, nil, 0, err
	}

	whiteStats := s.calculateDetailedStats(evaluations, chess.White)
	blackStats := s.calculateDetailedStats(evaluations, chess.Black)

	return whiteStats, blackStats, len(moves), nil
}

func (s *ChessAnalysisServer) evaluateAllMoves(ctx context.Context, originalGame *chess.Game, depth int) ([]MoveEvaluation, error) {
	moves := originalGame.Moves()
	var evaluations []MoveEvaluation

	// Create replay game
	replayGame := chess.NewGame()

	for i, move := range moves {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return nil, status.Error(codes.Canceled, "analysis canceled")
		default:
		}

		moveNum := i + 1
		position := replayGame.Position()
		phase := s.determineGamePhase(position, moveNum)
		playerColor := position.Turn()

		// Evaluate move
		centipawnLoss, err := s.evaluateSingleMove(replayGame, move, depth)
		if err != nil {
			log.Printf("Warning: Skipping move %d due to evaluation error: %v", moveNum, err)
		} else {
			evaluations = append(evaluations, MoveEvaluation{
				MoveNumber:    moveNum,
				Move:          move.String(),
				Phase:         phase,
				PlayerColor:   playerColor,
				CentipawnLoss: centipawnLoss,
			})
		}

		// Apply move for next iteration
		if err := replayGame.Move(move); err != nil {
			return nil, fmt.Errorf("failed to apply move %d: %w", moveNum, err)
		}

		// Progress logging every 20 moves
		if moveNum%20 == 0 {
			log.Printf("Analyzed %d/%d moves", moveNum, len(moves))
		}
	}

	log.Printf("Successfully analyzed %d moves", len(evaluations))
	return evaluations, nil
}

func (s *ChessAnalysisServer) evaluateSingleMove(game *chess.Game, move *chess.Move, depth int) (int, error) {
	engine := s.enginePool.Get()
	defer s.enginePool.Put(engine)

	// Evaluate current position
	cmdPos := uci.CmdPosition{Position: game.Position()}
	cmdGo := uci.CmdGo{Depth: depth}

	if err := engine.Run(cmdPos, cmdGo); err != nil {
		return 0, fmt.Errorf("failed to evaluate position: %w", err)
	}

	results := engine.SearchResults()
	bestMoveScore := results.Info.Score.CP

	// Evaluate position after the move
	testGame := chess.NewGame()
	for _, prevMove := range game.Moves() {
		if err := testGame.Move(prevMove); err != nil {
			return 0, fmt.Errorf("failed to replay game: %w", err)
		}
	}

	if err := testGame.Move(move); err != nil {
		return 0, fmt.Errorf("failed to apply move: %w", err)
	}

	cmdPosAfter := uci.CmdPosition{Position: testGame.Position()}
	if err := engine.Run(cmdPosAfter, cmdGo); err != nil {
		return 0, fmt.Errorf("failed to evaluate position after move: %w", err)
	}

	resultsAfter := engine.SearchResults()
	actualMoveScore := -resultsAfter.Info.Score.CP

	// Calculate centipawn loss
	centipawnLoss := bestMoveScore - actualMoveScore
	if centipawnLoss < 0 {
		centipawnLoss = 0
	}

	return centipawnLoss, nil
}

func (s *ChessAnalysisServer) determineGamePhase(pos *chess.Position, moveNumber int) GamePhase {
	if moveNumber <= 15 {
		return Opening
	}

	// Count non-pawn, non-king pieces
	pieceCount := 0
	board := pos.Board()
	for sq := chess.A1; sq <= chess.H8; sq++ {
		piece := board.Piece(sq)
		if piece != chess.NoPiece &&
			piece.Type() != chess.Pawn &&
			piece.Type() != chess.King {
			pieceCount++
		}
	}

	if pieceCount >= 6 {
		return Middlegame
	}
	return Endgame
}

func (s *ChessAnalysisServer) calculatePlayerElo(evaluations []MoveEvaluation, color chess.Color) *pb.PlayerEloAnalysis {
	phaseACPL := map[GamePhase][]int{
		Opening:    {},
		Middlegame: {},
		Endgame:    {},
	}

	for _, eval := range evaluations {
		if eval.PlayerColor == color {
			phaseACPL[eval.Phase] = append(phaseACPL[eval.Phase], eval.CentipawnLoss)
		}
	}

	return &pb.PlayerEloAnalysis{
		Opening:    int32(s.acplToElo(phaseACPL[Opening])),
		Middlegame: int32(s.acplToElo(phaseACPL[Middlegame])),
		Endgame:    int32(s.acplToElo(phaseACPL[Endgame])),
	}
}

func (s *ChessAnalysisServer) calculateDetailedStats(evaluations []MoveEvaluation, color chess.Color) *pb.PhaseStatsByPhase {
	phaseACPL := map[GamePhase][]int{
		Opening:    {},
		Middlegame: {},
		Endgame:    {},
	}

	for _, eval := range evaluations {
		if eval.PlayerColor == color {
			phaseACPL[eval.Phase] = append(phaseACPL[eval.Phase], eval.CentipawnLoss)
		}
	}

	return &pb.PhaseStatsByPhase{
		Opening:    s.calculatePhaseStats(phaseACPL[Opening]),
		Middlegame: s.calculatePhaseStats(phaseACPL[Middlegame]),
		Endgame:    s.calculatePhaseStats(phaseACPL[Endgame]),
	}
}

func (s *ChessAnalysisServer) calculatePhaseStats(losses []int) *pb.PhaseStats {
	if len(losses) == 0 {
		return &pb.PhaseStats{
			MoveCount:    0,
			TotalAcpl:    0,
			AverageAcpl:  0,
			EstimatedElo: 1500,
		}
	}

	total := 0
	for _, loss := range losses {
		total += loss
	}

	avg := float64(total) / float64(len(losses))

	return &pb.PhaseStats{
		MoveCount:    int32(len(losses)),
		TotalAcpl:    int32(total),
		AverageAcpl:  math.Round(avg*100) / 100,
		EstimatedElo: int32(s.acplToEloRating(avg)),
	}
}

func (s *ChessAnalysisServer) acplToElo(losses []int) int {
	if len(losses) == 0 {
		return 1500
	}

	total := 0
	for _, loss := range losses {
		total += loss
	}

	avgACPL := float64(total) / float64(len(losses))
	return s.acplToEloRating(avgACPL)
}

func (s *ChessAnalysisServer) acplToEloRating(acpl float64) int {
	switch {
	case acpl <= 5:
		return 2800
	case acpl <= 10:
		return 2600
	case acpl <= 15:
		return 2400
	case acpl <= 20:
		return 2200
	case acpl <= 30:
		return 2000
	case acpl <= 40:
		return 1800
	case acpl <= 55:
		return 1600
	case acpl <= 70:
		return 1400
	case acpl <= 90:
		return 1200
	case acpl <= 120:
		return 1000
	case acpl <= 150:
		return 800
	default:
		elo := int(1200 - (acpl-90)*4)
		if elo < 400 {
			return 400
		}
		return elo
	}
}

func findStockfish() (string, error) {
	if path := os.Getenv("STOCKFISH_PATH"); path != "" {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	possiblePaths := []string{
		"stockfish",
		"/usr/games/stockfish",
		"/usr/bin/stockfish",
		"/usr/local/bin/stockfish",
		"/opt/homebrew/bin/stockfish",
		"./stockfish",
		"./stockfish.exe",
	}

	for _, path := range possiblePaths {
		if fullPath, err := exec.LookPath(path); err == nil {
			return fullPath, nil
		}
	}

	return "", fmt.Errorf("stockfish executable not found")
}

func main() {
	log.Println("Starting Chess Analysis gRPC Microservice...")

	// Find Stockfish
	stockfishPath, err := findStockfish()
	if err != nil {
		log.Fatalf("Stockfish not found: %v", err)
	}
	log.Printf("Using Stockfish at: %s", stockfishPath)

	// Create engine pool
	enginePool, err := NewEnginePool(stockfishPath, enginePoolSize)
	if err != nil {
		log.Fatalf("Failed to create engine pool: %v", err)
	}
	defer enginePool.Close()

	// Create gRPC server
	server := grpc.NewServer()
	chessService := NewChessAnalysisServer(enginePool)
	pb.RegisterChessAnalysisServiceServer(server, chessService)

	// Listen on port
	port := defaultPort
	if envPort := os.Getenv("GRPC_PORT"); envPort != "" {
		port = ":" + envPort
	}

	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", port, err)
	}

	log.Printf("Chess Analysis gRPC service listening on %s", port)
	log.Printf("Engine pool initialized with %d Stockfish instances", enginePoolSize)

	// Start server in goroutine
	go func() {
		if err := server.Serve(lis); err != nil {
			log.Fatalf("Failed to serve gRPC server: %v", err)
		}
	}()

	// Wait for interrupt signal for graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down Chess Analysis gRPC service...")
	server.GracefulStop()
	log.Println("Service stopped")
}
