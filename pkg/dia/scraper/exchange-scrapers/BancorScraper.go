package scrapers

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"
	"time"

	ConverterRegistry "github.com/diadata-org/diadata/pkg/dia/scraper/exchange-scrapers/bancor"
	"github.com/diadata-org/diadata/pkg/dia/scraper/exchange-scrapers/bancor/BancorNetwork"
	"github.com/diadata-org/diadata/pkg/dia/scraper/exchange-scrapers/bancor/ConverterTypeFour"
	ConvertertypeOne "github.com/diadata-org/diadata/pkg/dia/scraper/exchange-scrapers/bancor/ConverterTypeOne"
	"github.com/diadata-org/diadata/pkg/dia/scraper/exchange-scrapers/bancor/ConverterTypeThree"
	"github.com/diadata-org/diadata/pkg/dia/scraper/exchange-scrapers/bancor/ConverterTypeZero"
	uniswapcontract "github.com/diadata-org/diadata/pkg/dia/scraper/exchange-scrapers/uniswap"

	"github.com/diadata-org/diadata/pkg/dia"
	"github.com/diadata-org/diadata/pkg/utils"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

type BancorPool struct {
	Reserves []struct {
		DltID   string `json:"dlt_id"`
		Symbol  string `json:"symbol"`
		Name    string `json:"name"`
		Balance struct {
			Usd string `json:"usd"`
		} `json:"balance"`
		Weight int `json:"weight"`
		Price  struct {
			Usd string `json:"usd"`
		} `json:"price"`
		Price24HAgo struct {
			Usd string `json:"usd"`
		} `json:"price_24h_ago"`
		Volume24H struct {
			Usd  string `json:"usd"`
			Base string `json:"base"`
		} `json:"volume_24h"`
	} `json:"reserves"`
	DltType        string `json:"dlt_type"`
	DltID          string `json:"dlt_id"`
	Type           int    `json:"type"`
	Version        int    `json:"version"`
	Symbol         string `json:"symbol"`
	Name           string `json:"name"`
	Supply         string `json:"supply"`
	ConverterDltID string `json:"converter_dlt_id"`
	ConversionFee  string `json:"conversion_fee"`
	Liquidity      struct {
		Usd string `json:"usd"`
	} `json:"liquidity"`
	Volume24H struct {
		Usd string `json:"usd"`
	} `json:"volume_24h"`
	Fees24H struct {
		Usd string `json:"usd"`
	} `json:"fees_24h"`
}

type BancorPools struct {
	Data      []BancorPool `json:"data"`
	Timestamp struct {
		Ethereum struct {
			Block     int   `json:"block"`
			Timestamp int64 `json:"timestamp"`
		} `json:"ethereum"`
	} `json:"timestamp"`
}

type BancorSwap struct {
	Pair       dia.ExchangePair
	FromAmount float64
	ToAmount   float64
	ID         string
	Timestamp  int64
}

type BancorScraper struct {
	WsClient   *ethclient.Client
	RestClient *ethclient.Client

	exchangeName string

	// channels to signal events
	run          bool
	initDone     chan nothing
	shutdown     chan nothing
	shutdownDone chan nothing

	errorLock sync.RWMutex
	error     error
	closed    bool

	pairScrapers   map[string]*BancorPairScraper
	productPairIds map[string]int
	chanTrades     chan *dia.Trade
}

func NewBancorScraper(exchange dia.Exchange, scrape bool) *BancorScraper {
	var wsClient, restClient *ethclient.Client
	var err error

	restClient, err = ethclient.Dial(utils.Getenv("ETH_URI_REST", restDialEth))
	if err != nil {
		log.Fatal(err)
	}

	wsClient, err = ethclient.Dial(utils.Getenv("ETH_URI_WS", wsDialEth))
	if err != nil {
		log.Fatal(err)
	}

	scraper := &BancorScraper{
		exchangeName:   exchange.Name,
		WsClient:       wsClient,
		RestClient:     restClient,
		initDone:       make(chan nothing),
		shutdown:       make(chan nothing),
		shutdownDone:   make(chan nothing),
		productPairIds: make(map[string]int),
		pairScrapers:   make(map[string]*BancorPairScraper),
		chanTrades:     make(chan *dia.Trade),
	}

	if scrape {
		go scraper.mainLoop()
	}
	return scraper
}

func (scraper *BancorScraper) mainLoop() {

	scraper.GetpoolAddress()

	sink, err := scraper.GetConversion()
	if err != nil {
		log.Errorln("Error GetConversion", err)
	}

	go func() {
		for {

			rawSwap := <-sink
			revRawSwap := reverseBNTSwap(*rawSwap)

			var address []common.Address
			swap, err := scraper.normalizeSwap(revRawSwap)
			if err != nil {
				log.Error("error normalizeSwap: ", err)

			}

			price, volume := scraper.getSwapData(swap)
			address = append(address, revRawSwap.FromToken)
			address = append(address, revRawSwap.ToToken)

			pair := scraper.GetPair(address)

			trade := &dia.Trade{
				Symbol:         pair.Symbol,
				Pair:           pair.ForeignName,
				Price:          price,
				Volume:         volume,
				Time:           time.Now(),
				ForeignTradeID: revRawSwap.Raw.TxHash.String(),
				Source:         scraper.exchangeName,
				BaseToken:      pair.UnderlyingPair.BaseToken,
				QuoteToken:     pair.UnderlyingPair.QuoteToken,
				VerifiedPair:   true,
			}

			log.Info("Got Trade: ", trade)
			scraper.chanTrades <- trade

		}
	}()
	if scraper.error == nil {
		scraper.error = errors.New("Main loop terminated by Close().")
	}
	scraper.cleanup(nil)
}

// Reverse swap involving BNT such that pair is always XXX-BNT.
func reverseBNTSwap(rawSwap BancorNetwork.BancorNetworkConversion) BancorNetwork.BancorNetworkConversion {
	var revRawSwap BancorNetwork.BancorNetworkConversion
	fmt.Println("raw swap ToToken: ", rawSwap.ToToken.Hex())
	if rawSwap.FromToken.Hex() == "0x1F573D6Fb3F13d689FF844B4cE37794d79a7FF1C" {
		revRawSwap.ToAmount = rawSwap.FromAmount
		revRawSwap.FromAmount = rawSwap.ToAmount
		revRawSwap.FromToken = rawSwap.ToToken
		revRawSwap.ToToken = rawSwap.FromToken
		revRawSwap.Raw = rawSwap.Raw
		return revRawSwap
	}
	return rawSwap
}

func (scraper *BancorScraper) GetpoolAddress() {
	// Get All Anchors

	converTerRegistryAddress := common.HexToAddress("0xC0205e203F423Bcd8B2a4d6f8C8A154b0Aa60F19")

	converter, err := ConverterRegistry.NewConverterRegistryCaller(converTerRegistryAddress, scraper.RestClient)
	if err != nil {
		log.Errorln("Error Getting Anchors", err)
	}

	_, err = converter.GetAnchors(&bind.CallOpts{})
	if err != nil {
		log.Errorln("Error Getting Anchors", err)
	}

}

func (scraper *BancorScraper) GetConversion() (chan *BancorNetwork.BancorNetworkConversion, error) {

	sink := make(chan *BancorNetwork.BancorNetworkConversion)

	var conversionFiltererContract *BancorNetwork.BancorNetworkFilterer

	address := common.HexToAddress("0x2F9EC37d6CcFFf1caB21733BdaDEdE11c823cCB0") // bancor Network
	conversionFiltererContract, err := BancorNetwork.NewBancorNetworkFilterer(address, scraper.WsClient)
	if err != nil {
		return nil, err
	}

	subs, err := conversionFiltererContract.WatchConversion(&bind.WatchOpts{}, sink, []common.Address{}, []common.Address{}, []common.Address{})
	if err != nil {
		log.Error("error in get swaps channel: ", err)
	}
	log.Infoln("Subscribed", subs)

	return sink, nil

}

// normalizeUniswapSwap takes a swap as returned by the swap contract's channel and converts it to a UniswapSwap type
func (scrapper *BancorScraper) normalizeSwap(swap BancorNetwork.BancorNetworkConversion) (BancorSwap, error) {
	var normalizedSwap BancorSwap
	if swap.FromToken.Hex() == "0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE" {
		amount0, _ := new(big.Float).Quo(big.NewFloat(0).SetInt(swap.FromAmount), new(big.Float).SetFloat64(math.Pow10(18))).Float64()
		normalizedSwap.FromAmount = amount0
	} else {
		fromToken, err := uniswapcontract.NewIERC20(swap.FromToken, scrapper.RestClient)
		if err != nil {
			return normalizedSwap, err
		}
		fromTokenDecimal, err := fromToken.Decimals(&bind.CallOpts{})
		if err != nil {
			return normalizedSwap, err
		}
		decimals0 := int(fromTokenDecimal)
		amount0, _ := new(big.Float).Quo(big.NewFloat(0).SetInt(swap.FromAmount), new(big.Float).SetFloat64(math.Pow10(decimals0))).Float64()
		normalizedSwap.FromAmount = amount0
	}

	if swap.ToToken.Hex() == "0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE" {
		amount1, _ := new(big.Float).Quo(big.NewFloat(0).SetInt(swap.ToAmount), new(big.Float).SetFloat64(math.Pow10(18))).Float64()
		normalizedSwap.ToAmount = amount1
	} else {
		toToken, err := uniswapcontract.NewIERC20(swap.ToToken, scrapper.RestClient)
		if err != nil {
			return normalizedSwap, err
		}
		toTokenDecimal, err := toToken.Decimals(&bind.CallOpts{})
		if err != nil {
			return normalizedSwap, err
		}
		decimals1 := int(toTokenDecimal)
		amount1, _ := new(big.Float).Quo(big.NewFloat(0).SetInt(swap.ToAmount), new(big.Float).SetFloat64(math.Pow10(decimals1))).Float64()
		normalizedSwap.ToAmount = amount1
	}

	pair := scrapper.GetPair([]common.Address{swap.ToToken, swap.FromToken})
	pair, _ = scrapper.NormalizePair(pair)
	normalizedSwap.Pair = pair
	normalizedSwap.ID = swap.Raw.TxHash.Hex()
	normalizedSwap.Timestamp = time.Now().Unix()

	return normalizedSwap, nil
}

func (scrapper *BancorScraper) getSwapData(swap BancorSwap) (price float64, volume float64) {
	volume = swap.FromAmount
	price = swap.ToAmount / swap.FromAmount
	return
}

func (scraper *BancorScraper) NormalizePair(pair dia.ExchangePair) (dia.ExchangePair, error) {
	if pair.UnderlyingPair.BaseToken.Address == "0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE" {
		pair.UnderlyingPair.BaseToken.Address = "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"
	}
	if pair.UnderlyingPair.QuoteToken.Address == "0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE" {
		pair.UnderlyingPair.QuoteToken.Address = "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"
	}
	return pair, nil
}

func (scraper *BancorScraper) ConverterTypeZero(address common.Address) (tokenAddress []common.Address, err error) {

	contract, err := ConverterTypeZero.NewConverterTypeZeroCaller(address, scraper.RestClient)
	if err != nil {
		return
	}

	tokenCount, err := contract.ConnectorTokenCount(&bind.CallOpts{})
	if err != nil {
		return
	}

	if tokenCount == 2 {
		token1, err := contract.ConnectorTokens(&bind.CallOpts{}, big.NewInt(1))
		if err != nil {
			log.Println("Error", err)
		}
		tokenAddress = append(tokenAddress, token1)
		token2, err := contract.ConnectorTokens(&bind.CallOpts{}, big.NewInt(0))
		if err != nil {
			log.Println("Error", err)
		}
		tokenAddress = append(tokenAddress, token2)

	}

	return

}

func (scraper *BancorScraper) ConverterTypeOne(address common.Address) (tokenAddress []common.Address, err error) {

	contract, err := ConvertertypeOne.NewConvertertypeOne(address, scraper.RestClient)
	if err != nil {
		return
	}

	tokenCount, err := contract.ConnectorTokenCount(&bind.CallOpts{})
	if err != nil {
		return
	}

	if tokenCount == 2 {
		token1, err := contract.ConnectorTokens(&bind.CallOpts{}, big.NewInt(1))
		if err != nil {
			log.Println("Error", err)
		}
		tokenAddress = append(tokenAddress, token1)
		token2, err := contract.ConnectorTokens(&bind.CallOpts{}, big.NewInt(0))
		if err != nil {
			log.Println("Error", err)
		}
		tokenAddress = append(tokenAddress, token2)

	}

	return

}

func (scraper *BancorScraper) ConverterTypeThree(address common.Address) (tokenAddress []common.Address, err error) {

	contract, err := ConverterTypeThree.NewConverterTypeThree(address, scraper.RestClient)
	if err != nil {
		return
	}

	tokenCount, err := contract.ConnectorTokenCount(&bind.CallOpts{})
	if err != nil {
		return
	}

	log.Println("tokenCount", tokenCount)

	if tokenCount == 2 {
		token1, err := contract.ConnectorTokens(&bind.CallOpts{}, big.NewInt(1))
		if err != nil {
			log.Println("Error", err)
		}
		tokenAddress = append(tokenAddress, token1)
		token2, err := contract.ConnectorTokens(&bind.CallOpts{}, big.NewInt(0))
		if err != nil {
			log.Println("Error", err)
		}
		tokenAddress = append(tokenAddress, token2)

	}

	return

}

func (scraper *BancorScraper) ConverterTypeFour(address common.Address) (tokenAddress []common.Address, err error) {

	contract, err := ConverterTypeFour.NewConverterTypeFour(address, scraper.RestClient)
	if err != nil {
		return
	}

	tokenCount, err := contract.ConnectorTokenCount(&bind.CallOpts{})
	if err != nil {
		return
	}

	if tokenCount == 2 {
		token1, err := contract.ConnectorTokens(&bind.CallOpts{}, big.NewInt(1))
		if err != nil {
			log.Println("Error", err)
		}
		tokenAddress = append(tokenAddress, token1)
		token2, err := contract.ConnectorTokens(&bind.CallOpts{}, big.NewInt(0))
		if err != nil {
			log.Println("Error", err)
		}
		tokenAddress = append(tokenAddress, token2)

	}

	return

}

func (scraper *BancorScraper) FillSymbolData(symbol string) (dia.Asset, error) {
	return dia.Asset{Symbol: symbol}, nil
}

func (scraper *BancorScraper) FetchAvailablePairs() (pairs []dia.ExchangePair, err error) {
	pools, err := scraper.readPools()
	if err != nil {
		log.Error("Couldn't obtain Bancor product ids:", err)
	}
	for _, pool := range pools {
		var address []common.Address

		switch pool.Type {
		case 0:
			{
				address, err = scraper.ConverterTypeZero(common.HexToAddress(pool.ConverterDltID))
				if err != nil {
					log.Errorln("Error getting Address", err)
				}
			}
		case 1:
			{
				address, err = scraper.ConverterTypeOne(common.HexToAddress(pool.ConverterDltID))
				if err != nil {
					log.Errorln("Error getting Address", err)
				}
			}
		case 3:
			{
				address, err = scraper.ConverterTypeThree(common.HexToAddress(pool.ConverterDltID))
				if err != nil {
					log.Errorln("Error getting Address", err)
				}
			}
		case 4:
			{
				address, err = scraper.ConverterTypeFour(common.HexToAddress(pool.ConverterDltID))
				if err != nil {
					log.Errorln("Error getting Address", err)
				}
			}
		}

		pair := scraper.GetPair(address)

		if pair.Symbol != "" && strings.Split(pair.ForeignName, "-")[1] != "" {
			log.Println("found pair: ", pair)
			pairs = append(pairs, pair)
		}

	}

	return
}

func (scraper *BancorScraper) GetPair(address []common.Address) dia.ExchangePair {
	var (
		symbol0 string
		symbol1 string
	)

	token0, err := uniswapcontract.NewIERC20Caller(address[0], scraper.RestClient)
	if err != nil {
		log.Errorln("Error Getting token 0 ", err)
	}
	token1, err := uniswapcontract.NewIERC20Caller(address[1], scraper.RestClient)
	if err != nil {
		log.Errorln("Error Getting token 1 ", err)
	}

	if address[0].Hex() == "0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE" {
		address[0] = common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")
		symbol0 = "WETH"
	} else {
		symbol0, err = token0.Symbol(&bind.CallOpts{})
		if err != nil {
			log.Error(err)
		}
	}
	if address[1].Hex() == "0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE" {
		address[0] = common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")
		symbol1 = "WETH"
	} else {
		symbol1, err = token1.Symbol(&bind.CallOpts{})
		if err != nil {
			log.Error(err)
		}
	}

	basetoken := dia.Asset{
		Symbol:     symbol1,
		Address:    address[1].Hex(),
		Blockchain: dia.ETHEREUM,
	}
	quotetoken := dia.Asset{
		Symbol:     symbol0,
		Address:    address[0].Hex(),
		Blockchain: dia.ETHEREUM,
	}

	return dia.ExchangePair{
		ForeignName:    symbol0 + "-" + symbol1,
		Symbol:         symbol0,
		Exchange:       scraper.exchangeName,
		UnderlyingPair: dia.Pair{BaseToken: basetoken, QuoteToken: quotetoken},
	}
}

func (scraper *BancorScraper) readPools() ([]BancorPool, error) {
	var bpools BancorPools
	pairs, _, err := utils.GetRequest("https://api-v2.bancor.network/pools")
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(pairs, &bpools)
	if err != nil {
		log.Error("Error reading json", err)

	}
	return bpools.Data, nil

}

func (scraper *BancorScraper) ScrapePair(pair dia.ExchangePair) (PairScraper, error) {
	scraper.errorLock.RLock()
	defer scraper.errorLock.RUnlock()

	if scraper.error != nil {
		return nil, scraper.error
	}

	if scraper.closed {
		return nil, errors.New("BancorScraper is closed")
	}

	pairScraper := &BancorPairScraper{
		parent: scraper,
		pair:   pair,
	}

	scraper.pairScrapers[pair.ForeignName] = pairScraper

	return pairScraper, nil
}
func (scraper *BancorScraper) cleanup(err error) {
	scraper.errorLock.Lock()
	defer scraper.errorLock.Unlock()
	if err != nil {
		scraper.error = err
	}
	scraper.closed = true
	close(scraper.shutdownDone)
}

func (scraper *BancorScraper) Close() error {
	// close the pair scraper channels
	scraper.run = false
	for _, pairScraper := range scraper.pairScrapers {
		pairScraper.closed = true
	}

	close(scraper.shutdown)
	<-scraper.shutdownDone
	return nil
}

type BancorPairScraper struct {
	parent *BancorScraper
	pair   dia.ExchangePair
	closed bool
}

func (pairScraper *BancorPairScraper) Pair() dia.ExchangePair {
	return pairScraper.pair
}

func (scraper *BancorScraper) Channel() chan *dia.Trade {
	return scraper.chanTrades
}

func (pairScraper *BancorPairScraper) Error() error {
	s := pairScraper.parent
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()
	return s.error
}

func (pairScraper *BancorPairScraper) Close() error {
	pairScraper.parent.errorLock.RLock()
	defer pairScraper.parent.errorLock.RUnlock()
	pairScraper.closed = true
	return nil
}
