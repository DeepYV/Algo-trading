package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"
)

var stockPurchaseAmount = 10000

// Stock data structure
type StockPrice struct {
	Status  string `json:"status"`
	Payload struct {
		AveragePrice      interface{} `json:"average_price"`
		DayChangePerc     float64     `json:"day_change_perc"`
		UpperCircuitLimit float64     `json:"upper_circuit_limit"`
		LowerCircuitLimit float64     `json:"lower_circuit_limit"`
		Ohlc              struct {
			Open  float64 `json:"open"`
			High  float64 `json:"high"`
			Low   float64 `json:"low"`
			Close float64 `json:"close"`
		} `json:"ohlc"`
		Depth struct {
			Buy  []Order `json:"buy"`
			Sell []Order `json:"sell"`
		} `json:"depth"`
		LastPrice         float64 `json:"last_price"`
		TotalBuyQuantity  float64 `json:"total_buy_quantity"`
		TotalSellQuantity float64 `json:"total_sell_quantity"`
		Volume            int     `json:"volume"`
	} `json:"payload"`
}

type Order struct {
	Price    float64 `json:"price"`
	Quantity int     `json:"quantity"`
}

type AnalysisResponse struct {
	Recommendation string   `json:"recommendation"`
	Confidence     string   `json:"confidence"`
	Reasons        []string `json:"reasons"`
	PriceTargets   struct {
		Short  float64 `json:"short_term"`
		Medium float64 `json:"medium_term"`
	} `json:"price_targets"`
	RiskLevel string `json:"risk_level"`
	Timestamp string `json:"timestamp"`
}

type OrderRequest struct {
	TradingSymbol    string  `json:"trading_symbol"`
	Quantity         int     `json:"quantity"`
	Price            float64 `json:"price,omitempty"`
	TriggerPrice     float64 `json:"trigger_price,omitempty"`
	Validity         string  `json:"validity"`
	Exchange         string  `json:"exchange"`
	Segment          string  `json:"segment"`
	Product          string  `json:"product"`
	OrderType        string  `json:"order_type"`
	TransactionType  string  `json:"transaction_type"`
	OrderReferenceID string  `json:"order_reference_id,omitempty"`
}

type OrderResponse struct {
	Status  string `json:"status"`
	Payload struct {
		OrderID string `json:"order_id"`
	} `json:"payload"`
	Error string `json:"error,omitempty"`
}

type StockPosition struct {
	Symbol      string
	BuyPrice    float64
	Quantity    int
	StopLoss    float64
	BuyOrderID  string
	SellOrderID string
}

var (
	positions = make(map[string]*StockPosition)
	posMutex  sync.Mutex
)

func main() {
	symbols := []string{"TATAMOTORS", "RELIANCE"} // here stocks means you need stockPurchaseAmount * number of stocks i.e 20000

	buyChan := make(chan string, len(symbols))
	sellChan := make(chan *StockPosition, len(symbols))
	doneChan := make(chan *StockPosition, len(symbols))

	go buyMonitor(buyChan, sellChan)
	go sellMonitor(sellChan, doneChan)

	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for {
		// Feed all symbols to buy channel every 30 minutes
		for _, symbol := range symbols {
			posMutex.Lock()
			_, exists := positions[symbol]
			posMutex.Unlock()

			if !exists {
				buyChan <- symbol
			}
		}

		select {
		case position := <-doneChan:
			posMutex.Lock()
			delete(positions, position.Symbol)
			posMutex.Unlock()
			log.Printf("[%s] Position closed completely", position.Symbol)
		default:
		}

		<-ticker.C
	}
}

func buyMonitor(buyChan <-chan string, sellChan chan<- *StockPosition) {
	for symbol := range buyChan {
		go func(s string) {
			// Check if we already have this position
			posMutex.Lock()
			_, exists := positions[s]
			posMutex.Unlock()

			if exists {
				return
			}

			stock, err := getStockData(s)
			if err != nil {
				log.Printf("[%s] Buy check failed: %v", s, err)
				return
			}

			analysis, err := analyzeStock(stock)
			if err != nil {
				log.Printf("[%s] Analysis failed: %v", s, err)
				return
			}

			if analysis.Recommendation == "buy" && (analysis.Confidence == "medium" || analysis.Confidence == "high") {
				// Original quantity calculation
				quantity := int(stock.Payload.LastPrice / float64(stockPurchaseAmount))
				// Original stop loss calculation (0.2%)
				stopLoss := stock.Payload.LastPrice - (stock.Payload.LastPrice * 0.002)

				orderID, err := createGrowSLOrder(quantity, stock.Payload.LastPrice, stopLoss, s)
				if err != nil {
					log.Printf("[%s] Buy order failed: %v", s, err)
					return
				}

				position := &StockPosition{
					Symbol:     s,
					BuyPrice:   stock.Payload.LastPrice,
					Quantity:   quantity,
					StopLoss:   stopLoss,
					BuyOrderID: orderID,
				}

				// Add to positions
				posMutex.Lock()
				positions[s] = position
				posMutex.Unlock()

				log.Printf("[%s] BOUGHT - Price: %.2f, Qty: %d, SL: %.2f",
					s, stock.Payload.LastPrice, quantity, stopLoss)

				// Send to sell monitor
				sellChan <- position
			}
		}(symbol)
	}
}

func sellMonitor(sellChan <-chan *StockPosition, doneChan chan<- *StockPosition) {
	for position := range sellChan {
		go func(pos *StockPosition) {
			ticker := time.NewTicker(30 * time.Minute)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					// Get current data
					stock, err := getStockData(pos.Symbol)
					if err != nil {
						log.Printf("[%s] Sell check failed: %v", pos.Symbol, err)
						continue
					}

					currentPrice := stock.Payload.LastPrice
					analysis, err := analyzeStock(stock)
					if err != nil {
						log.Printf(" Analysis failed: %v", err)
						return
					}

					if analysis.Recommendation == "sell" && (analysis.Confidence == "medium" || analysis.Confidence == "high") {
						orderID, err := createGrowSLSellOrder(pos.Quantity, currentPrice, pos.StopLoss, pos.Symbol)
						if err != nil {
							log.Printf("[%s] Sell order failed: %v", pos.Symbol, err)
							continue
						}

						pos.SellOrderID = orderID
						profit := (currentPrice - pos.BuyPrice) * float64(pos.Quantity)

						log.Printf("[%s] SOLD  Profit: %.2f",
							pos.Symbol, profit)

						// Notify main loop
						doneChan <- pos
						return
					} else {
						log.Printf("[%s] HOLDING - Current: %.2f, Buy: %.2f, SL: %.2f",
							pos.Symbol, currentPrice, pos.BuyPrice, pos.StopLoss)
					}
				}
			}
		}(position)
	}
}

func getStockData(symbol string) (StockPrice, error) {
	var stock StockPrice

	client := &http.Client{Timeout: 10 * time.Second}
	url := fmt.Sprintf("https://api.groww.in/v1/live-data/quote?exchange=NSE&segment=CASH&trading_symbol=%s", symbol)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return stock, fmt.Errorf("create request failed: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+getGrowwAPIKey())

	resp, err := client.Do(req)
	if err != nil {
		return stock, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return stock, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(&stock); err != nil {
		return stock, fmt.Errorf("decode response failed: %w", err)
	}

	return stock, nil
}

func analyzeStock(stock StockPrice) (AnalysisResponse, error) {
	var analysis AnalysisResponse

	prompt := buildAnalysisPrompt(stock)
	geminiResponse, err := callGeminiAPI(prompt)
	if err != nil {
		return analysis, err
	}

	cleanResponse := strings.TrimSpace(geminiResponse)
	if strings.HasPrefix(cleanResponse, "```json") {
		cleanResponse = strings.TrimPrefix(cleanResponse, "```json")
		cleanResponse = strings.TrimSuffix(cleanResponse, "```")
	} else if strings.HasPrefix(cleanResponse, "```") {
		cleanResponse = strings.TrimPrefix(cleanResponse, "```")
		cleanResponse = strings.TrimSuffix(cleanResponse, "```")
	}

	if err := json.Unmarshal([]byte(cleanResponse), &analysis); err != nil {
		return analysis, fmt.Errorf("failed to parse analysis (response: %s): %w", cleanResponse, err)
	}

	if analysis.Recommendation == "" || analysis.Confidence == "" || analysis.RiskLevel == "" {
		return analysis, fmt.Errorf("incomplete analysis response: %+v", analysis)
	}

	analysis.Timestamp = time.Now().Format(time.RFC3339)
	return analysis, nil
}

func buildAnalysisPrompt(stock StockPrice) string {
	prompt := `Provide intraday trading analysis based on 30-minute interval charts in JSON format ONLY. Do not include any additional text or explanation outside the JSON structure.
Use stock price patterns and current news sentiment to support your analysis.
Input Data:
- Current Price: %.2f
- Daily Change: %.2f%%
- OHLC: Open %.2f, High %.2f, Low %.2f, Close %.2f
- Volume: %d
- Buy Pressure: %.2f
- Sell Pressure: %.2f
- Circuit Limits: Upper %.2f%%, Lower %.2f%%
Respond with ONLY this exact JSON format, without any markdown or additional text:
{
    "recommendation": "buy|sell|wait",
    "confidence": "low|medium|high",
    "reasons": ["reason1", "reason2", "reason3"],
    "price_targets": 0.0,  // target price within 3-5%% profit from current price
    "risk_level": "low|medium|high"
}`

	buyPressure := stock.Payload.TotalBuyQuantity
	sellPressure := stock.Payload.TotalSellQuantity
	upperCircuitPct := (stock.Payload.UpperCircuitLimit - stock.Payload.LastPrice) / stock.Payload.LastPrice * 100
	lowerCircuitPct := (stock.Payload.LastPrice - stock.Payload.LowerCircuitLimit) / stock.Payload.LastPrice * 100

	return fmt.Sprintf(prompt,
		stock.Payload.LastPrice, stock.Payload.DayChangePerc,
		stock.Payload.Ohlc.Open, stock.Payload.Ohlc.High, stock.Payload.Ohlc.Low, stock.Payload.Ohlc.Close,
		stock.Payload.Volume,
		buyPressure, sellPressure,
		upperCircuitPct, lowerCircuitPct)
}

func callGeminiAPI(prompt string) (string, error) {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(getGeminiAPIKey()))
	if err != nil {
		return "", fmt.Errorf("failed to create client: %w", err)
	}
	defer client.Close()

	modelNames := []string{"gemini-pro", "gemini-1.0-pro", "gemini-1.5-flash"}
	var lastErr error

	for _, modelName := range modelNames {
		model := client.GenerativeModel(modelName)
		model.SetTemperature(0.7)
		model.SetTopP(0.9)

		resp, err := model.GenerateContent(ctx, genai.Text(prompt))
		if err != nil {
			lastErr = err
			continue
		}

		if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
			if text, ok := resp.Candidates[0].Content.Parts[0].(genai.Text); ok {
				return string(text), nil
			}
		}
	}

	return "", fmt.Errorf("all model attempts failed. Last error: %w", lastErr)
}

func createGrowSLOrder(quantity int, limitPrice, stopLossPrice float64, TradingSymbol string) (string, error) {
	apiKey := getGrowwAPIKey()
	url := "https://api.groww.in/v1/order/create"
	product := "CNC"

	order := OrderRequest{
		TradingSymbol:    TradingSymbol,
		Quantity:         quantity,
		Price:            limitPrice,
		TriggerPrice:     stopLossPrice,
		Validity:         "DAY",
		Exchange:         "NSE",
		Segment:          "CASH",
		Product:          product,
		OrderType:        "SL",
		TransactionType:  "BUY",
		OrderReferenceID: fmt.Sprintf("sl_order_%d", time.Now().Unix()),
	}

	jsonBody, err := json.Marshal(order)
	if err != nil {
		return "", fmt.Errorf("error marshaling order: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("error making request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("order creation failed with status %d: %s", resp.StatusCode, string(body))
	}

	var orderResponse OrderResponse
	if err := json.Unmarshal(body, &orderResponse); err != nil {
		return "", fmt.Errorf("error parsing response: %w", err)
	}

	if orderResponse.Status != "success" {
		return "", fmt.Errorf("order creation failed: %s", orderResponse.Error)
	}

	return orderResponse.Payload.OrderID, nil
}

func createGrowSLSellOrder(quantity int, entryPrice, stopLossPrice float64, tradingSymbol string) (string, error) {
	apiKey := getGrowwAPIKey()
	url := "https://api.groww.in/v1/order/create"
	product := "CNC"

	order := OrderRequest{
		TradingSymbol:    tradingSymbol,
		Quantity:         quantity,
		Price:            entryPrice,
		TriggerPrice:     stopLossPrice,
		Validity:         "DAY",
		Exchange:         "NSE",
		Segment:          "CASH",
		Product:          product,
		OrderType:        "SL",
		TransactionType:  "SELL",
		OrderReferenceID: fmt.Sprintf("sl_sell_%d", time.Now().Unix()),
	}

	jsonBody, err := json.Marshal(order)
	if err != nil {
		return "", fmt.Errorf("error marshaling order: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("error making request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response: %w", err)
	}

	var orderResponse OrderResponse
	if err := json.Unmarshal(body, &orderResponse); err != nil {
		return "", fmt.Errorf("error parsing response: %w", err)
	}

	if resp.StatusCode != http.StatusOK || orderResponse.Status != "success" {
		return "", fmt.Errorf("order failed (status %d): %s", resp.StatusCode, orderResponse.Error)
	}

	return orderResponse.Payload.OrderID, nil
}

func getGrowwAPIKey() string {
	return "your_groww_api_key_here"
}
func getGeminiAPIKey() string {
	return "your_gemini_api_key_here"
}
