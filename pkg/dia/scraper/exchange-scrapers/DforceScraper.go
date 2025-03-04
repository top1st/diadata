package scrapers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sync"
	"time"

	"github.com/diadata-org/diadata/pkg/dia/scraper/exchange-scrapers/dforce"
	"github.com/diadata-org/diadata/pkg/dia/scraper/exchange-scrapers/dforce/token"

	"github.com/diadata-org/diadata/pkg/dia"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	dforceWsDial         = "ws://159.69.120.42:8546/"
	dforceRestDial       = "http://159.69.120.42:8545/"
	dforceLookBackBlocks = 6 * 60 * 24 * 20
)

type DforceToken struct {
	Symbol   string
	Decimals uint8
	Address  string
	Name     string
}

type DforceScraper struct {
	exchangeName string

	// channels to signal events
	run          bool
	initDone     chan nothing
	shutdown     chan nothing
	shutdownDone chan nothing

	errorLock sync.RWMutex
	error     error
	closed    bool

	pairScrapers   map[string]*DforcePairScraper
	productPairIds map[string]int
	chanTrades     chan *dia.Trade

	WsClient    *ethclient.Client
	RestClient  *ethclient.Client
	resubscribe chan nothing
	tokens      map[string]dia.Asset
	contract    common.Address
}

func NewDforceScraper(exchange dia.Exchange, scrape bool) *DforceScraper {
	scraper := &DforceScraper{
		contract:       common.HexToAddress(exchange.Contract),
		exchangeName:   exchange.Name,
		initDone:       make(chan nothing),
		shutdown:       make(chan nothing),
		shutdownDone:   make(chan nothing),
		productPairIds: make(map[string]int),
		pairScrapers:   make(map[string]*DforcePairScraper),
		chanTrades:     make(chan *dia.Trade),
		resubscribe:    make(chan nothing),
		tokens:         make(map[string]dia.Asset),
	}

	wsClient, err := ethclient.Dial(dforceWsDial)
	if err != nil {
		log.Fatal(err)
	}
	scraper.WsClient = wsClient
	restClient, err := ethclient.Dial(dforceRestDial)
	if err != nil {
		log.Fatal(err)
	}
	scraper.RestClient = restClient

	scraper.loadTokens()

	if scrape {
		go scraper.mainLoop()
	}
	return scraper
}

func (scraper *DforceScraper) loadTokens() {

	// added by hand because the symbol method returns a bytes32 instead of string
	scraper.tokens["0xeb269732ab75A6fD61Ea60b06fE994cD32a83549"] = dia.Asset{
		Symbol:     "USDx",
		Decimals:   18,
		Address:    "0xeb269732ab75A6fD61Ea60b06fE994cD32a83549",
		Blockchain: dia.ETHEREUM,
	}

	filterer, err := dforce.NewDforceFilterer(scraper.contract, scraper.WsClient)
	if err != nil {
		log.Error(err)

	}

	header, err := scraper.RestClient.HeaderByNumber(context.Background(), nil)
	if err != nil {
		log.Fatal(err)
	}
	startblock := header.Number.Uint64() - uint64(dforceLookBackBlocks)

	it, err := filterer.FilterSwap(&bind.FilterOpts{Start: startblock})
	if err != nil {
		log.Error(err)
	}

	for it.Next() {
		i, _ := scraper.loadTokenData(it.Event.Input)
		o, _ := scraper.loadTokenData(it.Event.Output)
		log.Printf("\n %v  -%v- %v -%v- %v %v",
			it.Event.Input.Hex(),
			i.Symbol, i.Decimals,
			o.Symbol, o.Decimals,
			it.Event.Output.Hex())
	}

}

func (scraper *DforceScraper) loadTokenData(tokenAddress common.Address) (dia.Asset, error) {
	tokenStr := tokenAddress.Hex()
	if foundToken, ok := (scraper.tokens[tokenStr]); ok {
		return foundToken, nil
	} else {
		tokenCaller, err := token.NewTokenCaller(tokenAddress, scraper.RestClient)
		if err != nil {
			log.Error(err)
		}
		symbol, err := tokenCaller.Symbol(&bind.CallOpts{})
		if err != nil {
			log.Error(err)
		}
		decimals, err := tokenCaller.Decimals(&bind.CallOpts{})
		if err != nil {
			log.Error(err)
		}
		name, err := tokenCaller.Name(&bind.CallOpts{})
		if err != nil {
			log.Error(err)
		}
		dfToken := dia.Asset{
			Symbol:     symbol,
			Name:       name,
			Decimals:   uint8(decimals.Int64()),
			Address:    tokenAddress.Hex(),
			Blockchain: dia.ETHEREUM,
		}
		scraper.tokens[tokenStr] = dfToken
		return dfToken, err
	}
}

func (scraper *DforceScraper) subscribeToTrades() error {

	filterer, err := dforce.NewDforceFilterer(scraper.contract, scraper.WsClient)
	if err != nil {
		log.Error(err)
		return err
	}
	header, err := scraper.RestClient.HeaderByNumber(context.Background(), nil)
	if err != nil {
		log.Fatal(err)
	}
	startblock := header.Number.Uint64() - uint64(25250)

	sink := make(chan *dforce.DforceSwap)
	sub, err := filterer.WatchSwap(&bind.WatchOpts{Start: &startblock}, sink)
	if err != nil {
		log.Error(err)
		return err
	}

	go func() {
		fmt.Println("Subscribed to trades")
		defer fmt.Println("Unsubscribed to trades")
		defer sub.Unsubscribe()
		subscribed := true

		for scraper.run && subscribed {

			select {
			case err = <-sub.Err():
				if err != nil {
					log.Error(err)
				}
				subscribed = false
				if scraper.run {
					scraper.resubscribe <- nothing{}
				}
			case trade := <-sink:
				scraper.processTrade(trade)
			}
		}
	}()

	return err
}

func (scraper *DforceScraper) processTrade(trade *dforce.DforceSwap) {
	symbol, foreignName, volume, price, token0, token1, err := scraper.getSwapDataDforce(trade)
	timestamp := time.Now().Unix()
	if err != nil {
		log.Error(err)
	} else {
		if pairScraper, ok := scraper.pairScrapers[foreignName]; ok {

			trade := &dia.Trade{
				Symbol:         symbol,
				Pair:           pairScraper.pair.ForeignName,
				Price:          price,
				Volume:         volume,
				Time:           time.Unix(timestamp, 0),
				ForeignTradeID: "",
				Source:         scraper.exchangeName,
				BaseToken:      token1,
				QuoteToken:     token0,
				VerifiedPair:   true,
			}
			pairScraper.parent.chanTrades <- trade
			fmt.Println("got trade: ", trade)
		}
	}

}

func (scraper *DforceScraper) FillSymbolData(symbol string) (dia.Asset, error) {
	return dia.Asset{}, nil
}

func (scraper *DforceScraper) mainLoop() {

	scraper.run = true

	err := scraper.subscribeToTrades()
	if err != nil {
		log.Error(err)
	}
	go func() {
		for scraper.run {
			<-scraper.resubscribe
			if scraper.run {
				fmt.Println("resubscribe...")
				err := scraper.subscribeToTrades()
				if err != nil {
					log.Error(err)
				}
			}
		}
	}()

	if scraper.run {
		if len(scraper.pairScrapers) == 0 {
			scraper.error = errors.New("no pairs to scrape provided")
			log.Error(scraper.error.Error())
		}
	}

	time.Sleep(10 * time.Second)

	if scraper.error == nil {
		scraper.error = errors.New("main loop terminated by Close()")
	}
	scraper.cleanup(nil)
}

// getSwapData returns the foreign name, volume and price of a swap
func (scraper *DforceScraper) getSwapDataDforce(s *dforce.DforceSwap) (symbol string, foreignName string, volume float64, price float64, buyToken, sellToken dia.Asset, err error) {
	buyToken, err = scraper.loadTokenData(s.Output)
	if err != nil {
		log.Error(err)
	}
	sellToken, err = scraper.loadTokenData(s.Input)
	if err != nil {
		log.Error(err)
	}
	buyDecimals := buyToken.Decimals
	sellDecimals := sellToken.Decimals

	amountOut, _ := new(big.Float).Quo(big.NewFloat(0).SetInt(s.OutputAmount), new(big.Float).SetFloat64(math.Pow10(int(buyDecimals)))).Float64()

	amountIn, _ := new(big.Float).Quo(big.NewFloat(0).SetInt(s.InputAmount), new(big.Float).SetFloat64(math.Pow10(int(sellDecimals)))).Float64()

	volume = amountOut
	price = amountIn / amountOut
	foreignName = buyToken.Symbol + "-" + sellToken.Symbol
	symbol = buyToken.Symbol
	return
}

func (scraper *DforceScraper) FetchAvailablePairs() (pairs []dia.ExchangePair, err error) {

	pairSet := make(map[string]struct{})
	for _, p1 := range scraper.tokens {
		for _, p2 := range scraper.tokens {
			token1 := p1
			token2 := p2
			if token1 != token2 {

				foreignName := token1.Symbol + "-" + token2.Symbol
				if _, ok := pairSet[foreignName]; !ok {
					pairs = append(pairs, dia.ExchangePair{
						Symbol:      token1.Symbol,
						ForeignName: foreignName,
						Exchange:    scraper.exchangeName,
					})
					pairSet[foreignName] = struct{}{}
				}

				foreignName = token2.Symbol + "-" + token1.Symbol
				if _, ok := pairSet[foreignName]; !ok {
					pairs = append(pairs, dia.ExchangePair{
						Symbol:      token2.Symbol,
						ForeignName: foreignName,
						Exchange:    scraper.exchangeName,
					})
					pairSet[foreignName] = struct{}{}
				}

			}
		}
	}
	return
}

func (scraper *DforceScraper) NormalizePair(pair dia.ExchangePair) (dia.ExchangePair, error) {
	return dia.ExchangePair{}, nil
}

func (scraper *DforceScraper) ScrapePair(pair dia.ExchangePair) (PairScraper, error) {
	scraper.errorLock.RLock()
	defer scraper.errorLock.RUnlock()

	if scraper.error != nil {
		return nil, scraper.error
	}

	if scraper.closed {
		return nil, errors.New("DforceScraper is closed")
	}

	pairScraper := &DforcePairScraper{
		parent: scraper,
		pair:   pair,
	}

	scraper.pairScrapers[pair.ForeignName] = pairScraper

	return pairScraper, nil
}
func (scraper *DforceScraper) cleanup(err error) {
	scraper.errorLock.Lock()
	defer scraper.errorLock.Unlock()
	if err != nil {
		scraper.error = err
	}
	scraper.closed = true
	close(scraper.shutdownDone)
}

func (scraper *DforceScraper) Close() error {
	// close the pair scraper channels
	scraper.run = false
	for _, pairScraper := range scraper.pairScrapers {
		pairScraper.closed = true
	}
	scraper.WsClient.Close()
	scraper.RestClient.Close()

	close(scraper.shutdown)
	<-scraper.shutdownDone
	return nil
}

type DforcePairScraper struct {
	parent *DforceScraper
	pair   dia.ExchangePair
	closed bool
}

func (pairScraper *DforcePairScraper) Pair() dia.ExchangePair {
	return pairScraper.pair
}

func (scraper *DforceScraper) Channel() chan *dia.Trade {
	return scraper.chanTrades
}

func (pairScraper *DforcePairScraper) Error() error {
	s := pairScraper.parent
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()
	return s.error
}

func (pairScraper *DforcePairScraper) Close() error {
	pairScraper.parent.errorLock.RLock()
	defer pairScraper.parent.errorLock.RUnlock()
	pairScraper.closed = true
	return nil
}
