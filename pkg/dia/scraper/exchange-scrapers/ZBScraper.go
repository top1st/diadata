package scrapers

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/diadata-org/diadata/pkg/dia"
	models "github.com/diadata-org/diadata/pkg/model"
	"github.com/diadata-org/diadata/pkg/utils"
	ws "github.com/gorilla/websocket"
)

var ZBSocketURL string = "wss://api.zb.live/websocket"

type ZBSubscribe struct {
	Event   string `json:"event"`
	Channel string `json:"channel"`
}

type ZBTradeResponse struct {
	DataType string `json:"dataType"`
	Data     []struct {
		Amount    string `json:"amount"`
		Price     string `json:"price"`
		Tid       int    `json:"tid"`
		Date      int    `json:"date"`
		Type      string `json:"type"`
		TradeType string `json:"trade_type"`
	} `json:"data"`
	Channel string `json:"channel"`
}

type ZBScraper struct {
	wsClient *ws.Conn
	// signaling channels for session initialization and finishing
	//initDone     chan nothing
	shutdown     chan nothing
	shutdownDone chan nothing
	// error handling; to read error or closed, first acquire read lock
	// only cleanup method should hold write lock
	errorLock sync.RWMutex
	error     error
	closed    bool
	// used to keep track of trading pairs that we subscribed to
	pairScrapers map[string]*ZBPairScraper
	exchangeName string
	chanTrades   chan *dia.Trade
	db           *models.RelDB
}

// NewZBScraper returns a new ZBScraper for the given pair
func NewZBScraper(exchange dia.Exchange, scrape bool, relDB *models.RelDB) *ZBScraper {

	s := &ZBScraper{
		shutdown:     make(chan nothing),
		shutdownDone: make(chan nothing),
		pairScrapers: make(map[string]*ZBPairScraper),
		exchangeName: exchange.Name,
		error:        nil,
		chanTrades:   make(chan *dia.Trade),
		db:           relDB,
	}

	ZBWsURL := utils.Getenv("ZB_WS_URL", ZBSocketURL)

	var wsDialer ws.Dialer
	SwConn, _, err := wsDialer.Dial(ZBWsURL, nil)
	if err != nil {
		println(err.Error())
	}
	s.wsClient = SwConn

	if scrape {
		go s.mainLoop()
	}
	return s
}

// runs in a goroutine until s is closed
func (s *ZBScraper) mainLoop() {

	for {

		message := &ZBTradeResponse{}

		if s.error = s.wsClient.ReadJSON(&message); s.error != nil {
			log.Error(s.error.Error())
			break
		}

		for _, trade := range message.Data {
			var exchangepair dia.ExchangePair
			ps, ok := s.pairScrapers[strings.TrimSuffix(message.Channel, "_trades")]
			if !ok {
				log.Error("unknown pair: " + message.Channel)
				continue
			}

			f64Price, err := strconv.ParseFloat(trade.Price, 64)
			if err != nil {
				log.Error("error parsing price: " + trade.Price)
				continue
			}

			f64Volume, err := strconv.ParseFloat(trade.Amount, 64)
			if err != nil {
				log.Error("error parsing volume: " + trade.Price)
				continue
			}

			if trade.Type == "sell" {
				f64Volume = -f64Volume
			}

			exchangepair, err = s.db.GetExchangePairCache(s.exchangeName, strings.TrimSuffix(message.Channel, "_trades"))
			if err != nil {
				log.Error(err)
			}

			t := &dia.Trade{
				Symbol:         ps.Pair().Symbol,
				Pair:           strings.TrimSuffix(message.Channel, "_trades"),
				Price:          f64Price,
				Volume:         f64Volume,
				Time:           time.Unix(int64(trade.Date), 0),
				ForeignTradeID: fmt.Sprint(trade.Tid),
				Source:         s.exchangeName,
				VerifiedPair:   exchangepair.Verified,
				BaseToken:      exchangepair.UnderlyingPair.BaseToken,
				QuoteToken:     exchangepair.UnderlyingPair.QuoteToken,
			}
			ps.parent.chanTrades <- t
			if exchangepair.Verified {
				log.Infoln("Got verified trade: ", t)
			}

		}
	}
	s.cleanup(s.error)
}

func (s *ZBScraper) NormalizePair(pair dia.ExchangePair) (dia.ExchangePair, error) {
	return dia.ExchangePair{}, nil
}

func (s *ZBScraper) cleanup(err error) {
	s.errorLock.Lock()
	defer s.errorLock.Unlock()

	if err != nil {
		s.error = err
	}
	s.closed = true

	close(s.shutdownDone)
}

// FillSymbolData is not used by DEX scrapers.
func (s *ZBScraper) FillSymbolData(symbol string) (dia.Asset, error) {
	return dia.Asset{Symbol: symbol}, nil
}

// Close closes any existing API connections, as well as channels of
// PairScrapers from calls to ScrapePair
func (s *ZBScraper) Close() error {

	if s.closed {
		return errors.New("ZBScraper: Already closed")
	}

	close(s.shutdown)
	err := s.wsClient.Close()
	if err != nil {
		return err
	}
	<-s.shutdownDone
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()
	return s.error
}

// ScrapePair returns a PairScraper that can be used to get trades for a single pair from
// this APIScraper
func (s *ZBScraper) ScrapePair(pair dia.ExchangePair) (PairScraper, error) {
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()

	if s.error != nil {
		return nil, s.error
	}

	if s.closed {
		return nil, errors.New("ZBScraper: Call ScrapePair on closed scraper")
	}

	ps := &ZBPairScraper{
		parent: s,
		pair:   pair,
	}

	s.pairScrapers[pair.ForeignName] = ps

	a := &ZBSubscribe{
		Event:   "addChannel",
		Channel: pair.ForeignName + "_trades",
	}

	if err := s.wsClient.WriteJSON(a); err != nil {
		fmt.Println(err.Error())
	}

	return ps, nil
}

// FetchAvailablePairs returns a list with all available trade pairs
func (s *ZBScraper) FetchAvailablePairs() (pairs []dia.ExchangePair, err error) {
	return []dia.ExchangePair{}, errors.New("FetchAvailablePairs() not implemented")
}

// ZBPairScraper implements PairScraper for ZB
type ZBPairScraper struct {
	parent *ZBScraper
	pair   dia.ExchangePair
	closed bool
}

// Close stops listening for trades of the pair associated with s
func (ps *ZBPairScraper) Close() error {
	ps.closed = true
	return nil
}

// Channel returns a channel that can be used to receive trades
func (ps *ZBScraper) Channel() chan *dia.Trade {
	return ps.chanTrades
}

// Error returns an error when the channel Channel() is closed
// and nil otherwise
func (ps *ZBPairScraper) Error() error {
	s := ps.parent
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()
	return s.error
}

// Pair returns the pair this scraper is subscribed to
func (ps *ZBPairScraper) Pair() dia.ExchangePair {
	return ps.pair
}
