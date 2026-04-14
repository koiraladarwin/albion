package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
	_ "modernc.org/sqlite"
)

type Price struct {
	ItemID       string `json:"item_id"`
	City         string `json:"city"`
	Quality      int    `json:"quality"`
	SellPriceMin int    `json:"sell_price_min"`
	SellPriceMax int    `json:"sell_price_max"`
	BuyPriceMin  int    `json:"buy_price_min"`
	BuyPriceMax  int    `json:"buy_price_max"`
}

type ArbitrageResult struct {
	ItemID      string
	ItemName    string
	BuyCity     string
	SellCity    string
	BuyPrice    int
	SellPrice   int
	Profit      int
	ProfitPct   float64
	SafeVolume  int
	TotalProfit int
}

func findBestArbitrage(db *sql.DB, minProfitPct float64) ([]ArbitrageResult, error) {

	type Row struct {
		ItemID   string
		ItemName string
		City     string
		Quality  int
		SellMin  int
		BuyMax   int
	}

	rows, err := db.Query(`
		SELECT 
			p.item_id,
			i.name,
			p.city,
			p.quality,
			p.sell_price_min,
			p.buy_price_max
		FROM prices p
		LEFT JOIN items i ON i.id = p.item_id
		WHERE p.sell_price_min > 0 OR p.buy_price_max > 0
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type Agg struct {
		ItemName  string
		BuyCity   string
		SellCity  string
		BuyPrice  int
		SellPrice int
	}

	best := make(map[string]Agg)

	for rows.Next() {
		var r Row
		if err := rows.Scan(
			&r.ItemID,
			&r.ItemName,
			&r.City,
			&r.Quality,
			&r.SellMin,
			&r.BuyMax,
		); err != nil {
			return nil, err
		}

		a := best[r.ItemID]

		if a.ItemName == "" {
			a.ItemName = r.ItemName
		}

		if r.SellMin > 0 {
			if a.BuyPrice == 0 || r.SellMin < a.BuyPrice {
				a.BuyPrice = r.SellMin
				a.BuyCity = r.City
			}
		}

		if r.BuyMax > 0 {
			if r.BuyMax > a.SellPrice {
				a.SellPrice = r.BuyMax
				a.SellCity = r.City
			}
		}

		best[r.ItemID] = a
	}

	results := make([]ArbitrageResult, 0)

	for itemID, a := range best {

		if a.BuyPrice == 0 || a.SellPrice == 0 {
			continue
		}

		profit := a.SellPrice - a.BuyPrice
		if profit <= 0 {
			continue
		}

		profitPct := float64(profit) * 100.0 / float64(a.BuyPrice)

		if profitPct < minProfitPct {
			continue
		}

		safeVolume := 1
		totalProfit := profit * safeVolume

		results = append(results, ArbitrageResult{
			ItemID:      itemID,
			ItemName:    a.ItemName,
			BuyCity:     a.BuyCity,
			SellCity:    a.SellCity,
			BuyPrice:    a.BuyPrice,
			SellPrice:   a.SellPrice,
			Profit:      profit,
			ProfitPct:   profitPct,
			SafeVolume:  safeVolume,
			TotalProfit: totalProfit,
		})
	}

	return results, nil
}

func scrapeAndStorePrices(db *sql.DB) error {

	db.Exec("PRAGMA journal_mode=WAL;")
	db.Exec("PRAGMA busy_timeout=5000;")

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	_, err = tx.Exec("DELETE FROM prices")
	if err != nil {
		tx.Rollback()
		return err
	}

	_, _ = tx.Exec("DELETE FROM sqlite_sequence WHERE name='prices'")

	if err := tx.Commit(); err != nil {
		return err
	}

	rows, err := db.Query("SELECT id FROM items")
	if err != nil {
		return err
	}
	defer rows.Close()

	client := &http.Client{}

	stmt, err := db.Prepare(`
		INSERT INTO prices (
			item_id, city, quality,
			sell_price_min, sell_price_max,
			buy_price_min, buy_price_max
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	var items []string

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		items = append(items, id)
	}

	batchSize := 150
	count := 0

	for i := 0; i < len(items); i += batchSize {
		end := i + batchSize
		if end > len(items) {
			end = len(items)
		}

		batch := items[i:end]
		count++

		fmt.Printf("\n[Batch %d] Scraping %d items...\n", count, len(batch))

		url := "https://europe.albion-online-data.com/api/v2/stats/prices/" +
			strings.Join(batch, ",")

		var resp *http.Response

		for {
			resp, err = client.Get(url)
			if err != nil {
				fmt.Println("❌ request error, retrying...")
				time.Sleep(2 * time.Second)
				continue
			}

			if resp.StatusCode != 200 {
				fmt.Println("❌ bad status:", resp.StatusCode, "retrying...")
				resp.Body.Close()
				time.Sleep(2 * time.Second)
				continue
			}

			break
		}

		var data []Price
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			resp.Body.Close()
			return err
		}
		resp.Body.Close()

		for _, p := range data {
			_, err := stmt.Exec(
				p.ItemID,
				p.City,
				p.Quality,
				p.SellPriceMin,
				p.SellPriceMax,
				p.BuyPriceMin,
				p.BuyPriceMax,
			)

			if err != nil {
				return err
			}
		}

		fmt.Println("✅ batch done")
		time.Sleep(1200 * time.Millisecond)
	}

	fmt.Println("\n🎉 Scraping complete")
	return nil
}

type item struct {
	id   string
	name string
}

func addToFavorites(db *sql.DB, itemId string) error {
	_, err := db.Exec(
		"INSERT OR IGNORE INTO favorites (item_id) VALUES (?)",
		itemId,
	)

	if err == nil {
		fmt.Println("Added to favorites:", itemId)
	}
	return err
}

func removeFromFavorites(db *sql.DB, itemId string) error {
	_, err := db.Exec(
		"DELETE FROM favorites WHERE item_id = ?",
		itemId,
	)

	if err == nil {
		fmt.Println("Removed from favorites:", itemId)
	}
	return err
}

func seeFavorites(db *sql.DB) error {
	rows, err := db.Query(`
		SELECT i.id, i.name
		FROM favorites f
		JOIN items i ON i.id = f.item_id
		ORDER BY i.name
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	fmt.Println("\n=== FAVORITES ===")

	found := false

	for rows.Next() {
		var id, name string
		err := rows.Scan(&id, &name)
		if err != nil {
			return err
		}

		fmt.Printf("%s (%s)\n", id, name)
		found = true
	}

	if !found {
		fmt.Println("No favorites yet")
	}

	return nil
}

func selectItems(reader *bufio.Reader, db *sql.DB) (string, error) {
	fmt.Print("Search: ")
	input, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}

	search := strings.TrimSpace(input)
	if search == "exit" {
		return "", nil
	}

	rows, err := db.Query(
		"SELECT id, name FROM items WHERE LOWER(name) LIKE LOWER(?) ORDER BY name",
		"%"+search+"%",
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var results []item

	for rows.Next() {
		var it item
		rows.Scan(&it.id, &it.name)
		results = append(results, it)
	}

	if len(results) == 0 {
		fmt.Println("No matches found")
		return "", nil
	}

	for i, v := range results {
		fmt.Printf("%d) %s (%s)\n", i, v.id, v.name)
	}

	fmt.Print("Select index: ")
	input, err = reader.ReadString('\n')
	if err != nil {
		return "", err
	}

	input = strings.TrimSpace(input)
	index, err := strconv.Atoi(input)
	if err != nil {
		return "", err
	}

	if index < 0 || index >= len(results) {
		return "", fmt.Errorf("invalid index")
	}

	return results[index].id, nil
}

func seeItems(reader *bufio.Reader, db *sql.DB) {
	selectItems(reader, db)
}

func editFavorites(reader *bufio.Reader, db *sql.DB) error {
	fmt.Println("1) Add Favorite")
	fmt.Println("2) Remove Favorite")
	fmt.Println("3) Back")
	fmt.Print("Choice: ")

	input, _ := reader.ReadString('\n')
	choice := strings.TrimSpace(input)

	switch choice {
	case "1":
		itemId, err := selectItems(reader, db)
		if err != nil {
			return err
		}
		if itemId != "" {
			return addToFavorites(db, itemId)
		}

	case "2":
		itemId, err := selectItems(reader, db)
		if err != nil {
			return err
		}
		if itemId != "" {
			return removeFromFavorites(db, itemId)
		}
	case "3":
		return nil
	}

	return nil
}

func handleArbitrageCLI(db *sql.DB, reader *bufio.Reader) error {

	fmt.Print("\nEnter minimum profit percentage (e.g. 20): ")
	pctStr, _ := reader.ReadString('\n')
	pctStr = strings.TrimSpace(pctStr)

	minPct, err := strconv.ParseFloat(pctStr, 64)
	if err != nil {
		return fmt.Errorf("invalid percentage: %v", err)
	}

	fmt.Println("\nChoose output mode:")
	fmt.Println("1) Print to terminal")
	fmt.Println("2) Export to Excel")
	fmt.Print("Enter choice: ")

	choice, _ := reader.ReadString('\n')
	choice = strings.TrimSpace(choice)

	results, err := findBestArbitrage(db, minPct)
	if err != nil {
		return err
	}

	switch choice {

	case "1":
		printArbitrage(results)

	case "2":
		return exportArbitrageExcel(results)

	default:
		fmt.Println("Invalid choice")
	}

	return nil
}
func printArbitrage(results []ArbitrageResult) {

	fmt.Println("\n🔥 ARBITRAGE OPPORTUNITIES")
	fmt.Println("================================================")

	for _, r := range results {
		fmt.Printf(
			"%s (%s)\nBuy: %s (%d)\nSell: %s (%d)\nProfit: %d (%.2f%%)\nTotal Profit: %d silver\nRoute: %s → %s\n\n",
			r.ItemName,
			r.ItemID,
			r.BuyCity, r.BuyPrice,
			r.SellCity, r.SellPrice,
			r.Profit,
			r.ProfitPct,
			r.TotalProfit,
			r.BuyCity,
			r.SellCity,
		)
	}
}

func exportArbitrageExcel(results []ArbitrageResult) error {

	f := excelize.NewFile()
	sheet := "Arbitrage"
	f.SetSheetName("Sheet1", sheet)

	headers := []string{
		"Item ID", "Item Name",
		"Buy City", "Buy Price",
		"Sell City", "Sell Price",
		"Profit", "Profit %",
		"Total Profit",
	}

	for i, h := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		f.SetCellValue(sheet, cell, h)
	}

	for i, r := range results {
		row := i + 2

		f.SetCellValue(sheet, fmt.Sprintf("A%d", row), r.ItemID)
		f.SetCellValue(sheet, fmt.Sprintf("B%d", row), r.ItemName)
		f.SetCellValue(sheet, fmt.Sprintf("C%d", row), r.BuyCity)
		f.SetCellValue(sheet, fmt.Sprintf("D%d", row), r.BuyPrice)
		f.SetCellValue(sheet, fmt.Sprintf("E%d", row), r.SellCity)
		f.SetCellValue(sheet, fmt.Sprintf("F%d", row), r.SellPrice)
		f.SetCellValue(sheet, fmt.Sprintf("G%d", row), r.Profit)
		f.SetCellValue(sheet, fmt.Sprintf("H%d", row), r.ProfitPct)
		f.SetCellValue(sheet, fmt.Sprintf("I%d", row), r.TotalProfit)
	}

	filename := "arbitrage.xlsx"
	if err := f.SaveAs(filename); err != nil {
		return err
	}

	fmt.Println("Excel exported:", filename)
	return nil
}

type Result struct {
	ItemName  string
	ItemID    string
	BuyCities string
	BuyPrice  int
	SellCity  string
	SellPrice int
	Profit    int
	ProfitPct float64
}

func placeholders(n int) string {
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}

func toInterface(arr []string) []interface{} {
	out := make([]interface{}, len(arr))
	for i, v := range arr {
		out[i] = v
	}
	return out
}

func handleFavoritesArbitrage(db *sql.DB, reader *bufio.Reader) ([]Result, error) {

	rows, err := db.Query(`SELECT item_id FROM favorites`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		items = append(items, id)
	}

	if len(items) == 0 {
		return nil, nil
	}

	cities := []string{
		"Lymhurst",
		"Bridgewatch",
		"Martlock",
		"Fort Sterling",
		"Thetford",
		"Caerleon",
		"Black Market",
	}

	fmt.Println("\n=== CITIES ===")
	for i, c := range cities {
		fmt.Printf("%d) %s\n", i+1, c)
	}

	fmt.Print("\nEnter BUY city indexes (e.g. 1,2,3): ")
	buyInput, _ := reader.ReadString('\n')
	buyInput = strings.TrimSpace(buyInput)

	buyIdxs := strings.Split(buyInput, ",")
	var buyCities []string

	for _, s := range buyIdxs {
		i, err := strconv.Atoi(strings.TrimSpace(s))
		if err == nil && i >= 1 && i <= len(cities) {
			buyCities = append(buyCities, cities[i-1])
		}
	}

	if len(buyCities) == 0 {
		return nil, fmt.Errorf("no valid BUY cities selected")
	}

	fmt.Print("Enter SELL city index: ")
	sellInput, _ := reader.ReadString('\n')
	sellInput = strings.TrimSpace(sellInput)

	sellIdx, err := strconv.Atoi(sellInput)
	if err != nil || sellIdx < 1 || sellIdx > len(cities) {
		return nil, fmt.Errorf("invalid SELL city")
	}
	sellCity := cities[sellIdx-1]

	var results []Result

	for _, item := range items {

		var buy sql.NullInt64
		var itemName string

		buyQuery := `
			SELECT 
				MIN(NULLIF(p.sell_price_min, 0)),
				i.name
			FROM prices p
			LEFT JOIN items i ON i.id = p.item_id
			WHERE p.item_id = ?
			AND p.city IN (` + placeholders(len(buyCities)) + `)
			GROUP BY i.name
		`

		args := append([]interface{}{item}, toInterface(buyCities)...)

		err := db.QueryRow(buyQuery, args...).Scan(&buy, &itemName)
		if err != nil || !buy.Valid {
			continue
		}

		var sell sql.NullInt64

		err = db.QueryRow(`
			SELECT MAX(NULLIF(buy_price_max, 0))
			FROM prices
			WHERE item_id = ?
			AND city = ?
		`, item, sellCity).Scan(&sell)

		if err != nil || !sell.Valid {
			continue
		}

		profit := int(sell.Int64 - buy.Int64)
		profitPct := (float64(profit) * 100.0) / float64(buy.Int64)

		results = append(results, Result{
			ItemName:  itemName,
			ItemID:    item,
			BuyCities: strings.Join(buyCities, ","),
			BuyPrice:  int(buy.Int64),
			SellCity:  sellCity,
			SellPrice: int(sell.Int64),
			Profit:    profit,
			ProfitPct: profitPct,
		})
	}

	return results, nil
}

func printResults(results []Result) {
	fmt.Println("\n🔥 ARBITRAGE RESULTS")
	fmt.Println("================================")

	for _, r := range results {
		fmt.Printf(
			"%s(%s)\nBuy: %s (%d)\nSell: %s (%d)\nProfit: %d (%.2f%%)\n\n",
			r.ItemID,
			r.ItemName,
			r.BuyCities,
			r.BuyPrice,
			r.SellCity,
			r.SellPrice,
			r.Profit,
			r.ProfitPct,
		)
	}
}
func exportResultsTxt(results []Result, filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	w := bufio.NewWriter(file)

	w.WriteString("🔥 ARBITRAGE RESULTS\n")
	w.WriteString("================================\n\n")

	for _, r := range results {
		fmt.Fprintf(w,
			"%s (%s)\nBuy: %s (%d)\nSell: %s (%d)\nProfit: %d (%.2f%%)\n\n",
			r.ItemID,
			r.ItemName,
			r.BuyCities,
			r.BuyPrice,
			r.SellCity,
			r.SellPrice,
			r.Profit,
			r.ProfitPct,
		)
	}

	return w.Flush()
}

func handleFavoritesAribitageCLI(db *sql.DB, reader *bufio.Reader) error {

	results, err := handleFavoritesArbitrage(db, reader)
	if err != nil {
		return err
	}

	fmt.Println("\nChoose output:")
	fmt.Println("1) Print")
	fmt.Println("2) Export to text file")
	fmt.Println("3) Exit")
	fmt.Print("Choice: ")

	choice, _ := reader.ReadString('\n')
	choice = strings.TrimSpace(choice)

	switch choice {

	case "1":
		printResults(results)

	case "2":
		filename := "arbitrage_results.txt"
		err := exportResultsTxt(results, filename)
		if err != nil {
			return err
		}
		fmt.Println("Saved to", filename)

	default:
		fmt.Println("Exit")
	}

	return nil
}

func importItemsFromAlbion(db *sql.DB) error {

	url := "https://cdn.albionfreemarket.com/AlbionFormattedItemsParser/us_name_mappings.json"

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("failed request: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	items := make(map[string]string)
	if err := json.Unmarshal(body, &items); err != nil {
		return err
	}

	stmt, err := db.Prepare(`
		INSERT OR REPLACE INTO items (id, name)
		VALUES (?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	count := 0

	for id, name := range items {
		if id == "" || name == "" {
			continue
		}

		_, err := stmt.Exec(id, name)
		if err != nil {
			return err
		}

		count++
	}

	fmt.Printf("✅ Imported %d items\n", count)
	return nil
}

func initDB(db *sql.DB) error {

	queries := []string{
		`CREATE TABLE IF NOT EXISTS items (
			id TEXT PRIMARY KEY,
			name TEXT
		);`,

		`CREATE TABLE IF NOT EXISTS favorites (
			item_id TEXT PRIMARY KEY,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (item_id) REFERENCES items(id)
		);`,

		`CREATE TABLE IF NOT EXISTS prices (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			item_id TEXT NOT NULL,
			city TEXT NOT NULL,
			quality INTEGER NOT NULL,
			sell_price_min INTEGER,
			sell_price_max INTEGER,
			buy_price_min INTEGER,
			buy_price_max INTEGER,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (item_id) REFERENCES items(id)
		);`,
	}

	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}

	return nil
}

func main() {
	db, err := sql.Open("sqlite", "items.db")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	if err := initDB(db); err != nil {
		panic(err)
	}

	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Println("\n==== MENU ====")
		fmt.Println("1) Scrape All Items")
		fmt.Println("2) Search Items")
		fmt.Println("3) Edit Favorites")
		fmt.Println("4) See Favourites")
		fmt.Println("5) Scrape All Items Prices")
		fmt.Println("6) Find Best Guaranteed")
		fmt.Println("7) Favourite Arbitage")
		fmt.Println("8) Exit")
		fmt.Print("Choice: ")

		input, _ := reader.ReadString('\n')
		choice := strings.TrimSpace(input)

		switch choice {
		case "1":
			if err := importItemsFromAlbion(db); err != nil {
				panic(err)
			}

		case "2":
			seeItems(reader, db)

		case "3":
			err := editFavorites(reader, db)
			if err != nil {
				fmt.Println("Error:", err)
			}
		case "4":
			err := seeFavorites(db)
			if err != nil {
				fmt.Println("Error:", err)
			}
		case "5":
			err := scrapeAndStorePrices(db)
			if err != nil {
				fmt.Println("Error:", err)
			}

		case "6":
			err := handleArbitrageCLI(db, reader)
			if err != nil {
				fmt.Println("Error:", err)
			}
		case "7":
			err := handleFavoritesAribitageCLI(db, reader)
			if err != nil {
				fmt.Println("Error:", err)
			}
		case "8":
			fmt.Println("Bye!")
			return

		default:
			fmt.Println("Invalid choice")
		}
	}
}
