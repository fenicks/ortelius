// (c) 2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package avm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/gocraft/dbr/v2"

	"github.com/ava-labs/ortelius/services"
	"github.com/ava-labs/ortelius/services/indexes/models"
	"github.com/ava-labs/ortelius/services/indexes/params"
)

const (
	MaxAggregateIntervalCount = 20000
	MinSearchQueryLength      = 1
)

var (
	ErrAggregateIntervalCountTooLarge = errors.New("requesting too many intervals")
	ErrFailedToParseStringAsBigInt    = errors.New("failed to parse string to big.Int")
	ErrSearchQueryTooShort            = errors.New("search query too short")
)

var (
	outputSelectColumns = []string{
		"avm_outputs.id",
		"avm_outputs.transaction_id",
		"avm_outputs.output_index",
		"avm_outputs.asset_id",
		"avm_outputs.output_type",
		"avm_outputs.amount",
		"avm_outputs.locktime",
		"avm_outputs.threshold",
		"avm_outputs.created_at",
		"avm_outputs.redeeming_transaction_id",
		"avm_outputs.group_id",
		"avm_outputs.payload",
	}
)

type Reader struct {
	chainID string
	conns   *services.Connections
}

func NewReader(conns *services.Connections, chainID string) *Reader {
	return &Reader{
		conns:   conns,
		chainID: chainID,
	}
}

func (r *Reader) Search(ctx context.Context, p *params.SearchParams) (*models.SearchResults, error) {
	if len(p.Query) < MinSearchQueryLength {
		return nil, ErrSearchQueryTooShort
	}

	// See if the query string is an id or shortID. If so we can search on them
	// directly. Otherwise we treat the query as a normal query-string.
	if shortID, err := params.AddressFromString(p.Query); err == nil {
		return r.searchByShortID(ctx, shortID)
	}
	if id, err := ids.FromString(p.Query); err == nil {
		return r.searchByID(ctx, id)
	}

	// copy the list params, and inject DisableCounting for subsequent List* calls.
	cpListParams := p.ListParams
	cpListParams.DisableCounting = true

	// The query string was not an id/shortid so perform a regular search against
	// all models
	assets, err := r.ListAssets(ctx, &params.ListAssetsParams{ListParams: cpListParams, Query: p.Query})
	if err != nil {
		return nil, err
	}
	if len(assets.Assets) >= p.Limit {
		return collateSearchResults(assets, nil, nil, nil)
	}

	transactions, err := r.ListTransactions(ctx, &params.ListTransactionsParams{ListParams: cpListParams, Query: p.Query})
	if err != nil {
		return nil, err
	}
	if len(transactions.Transactions) >= p.Limit {
		return collateSearchResults(assets, nil, transactions, nil)
	}

	addresses, err := r.ListAddresses(ctx, &params.ListAddressesParams{ListParams: cpListParams, Query: p.Query})
	if err != nil {
		return nil, err
	}
	if len(addresses.Addresses) >= p.Limit {
		return collateSearchResults(assets, addresses, transactions, nil)
	}

	return collateSearchResults(assets, addresses, transactions, nil)
}

func (r *Reader) Aggregate(ctx context.Context, params *params.AggregateParams) (*models.AggregatesHistogram, error) {
	// Validate params and set defaults if necessary
	if params.StartTime.IsZero() {
		var err error
		params.StartTime, err = r.getFirstTransactionTime(ctx, params.ChainIDs)
		if err != nil {
			return nil, err
		}
	}

	// Ensure the interval count requested isn't too large
	intervalSeconds := int64(params.IntervalSize.Seconds())
	requestedIntervalCount := 0
	if intervalSeconds != 0 {
		requestedIntervalCount = int(math.Ceil(params.EndTime.Sub(params.StartTime).Seconds() / params.IntervalSize.Seconds()))
		if requestedIntervalCount > MaxAggregateIntervalCount {
			return nil, ErrAggregateIntervalCountTooLarge
		}
		if requestedIntervalCount < 1 {
			requestedIntervalCount = 1
		}
	}

	// Build the query and load the base data
	dbRunner := r.conns.DB().NewSession("get_transaction_aggregates_histogram")

	columns := []string{
		"COALESCE(SUM(avm_outputs.amount), 0) AS transaction_volume",

		"COUNT(DISTINCT(avm_outputs.transaction_id)) AS transaction_count",
		"COUNT(DISTINCT(avm_output_addresses.address)) AS address_count",
		"COUNT(DISTINCT(avm_outputs.asset_id)) AS asset_count",
		"COUNT(avm_outputs.id) AS output_count",
	}

	if requestedIntervalCount > 0 {
		columns = append(columns, fmt.Sprintf(
			"FLOOR((UNIX_TIMESTAMP(avm_outputs.created_at)-%d) / %d) AS idx",
			params.StartTime.Unix(),
			intervalSeconds))
	}

	builder := params.Apply(dbRunner.
		Select(columns...).
		From("avm_outputs").
		LeftJoin("avm_output_addresses", "avm_output_addresses.output_id = avm_outputs.id"))

	if requestedIntervalCount > 0 {
		builder.
			GroupBy("idx").
			OrderAsc("idx").
			Limit(uint64(requestedIntervalCount))
	}

	intervals := []models.Aggregates{}
	_, err := builder.LoadContext(ctx, &intervals)
	if err != nil {
		return nil, err
	}

	// If no intervals were requested then the total aggregate is equal to the
	// first (and only) interval, and we're done
	if requestedIntervalCount == 0 {
		// This check should never fail if the SQL query is correct, but added for
		// robustness to prevent panics if the invariant does not hold.
		if len(intervals) > 0 {
			intervals[0].StartTime = params.StartTime
			intervals[0].EndTime = params.EndTime
			return &models.AggregatesHistogram{Aggregates: intervals[0]}, nil
		}
		return &models.AggregatesHistogram{}, nil
	}

	// We need to return multiple intervals so build them now.
	// Intervals without any data will not return anything so we pad our results
	// with empty aggregates.
	//
	// We also add the start and end times of each interval to that interval
	aggs := &models.AggregatesHistogram{IntervalSize: params.IntervalSize}

	var startTS int64
	timesForInterval := func(intervalIdx int) (time.Time, time.Time) {
		// An interval's start Time is its index Time the interval size, plus the
		// starting Time. The end Time is (interval size - 1) seconds after the
		// start Time.
		startTS = params.StartTime.Unix() + (int64(intervalIdx) * intervalSeconds)
		return time.Unix(startTS, 0).UTC(),
			time.Unix(startTS+intervalSeconds-1, 0).UTC()
	}

	padTo := func(slice []models.Aggregates, to int) []models.Aggregates {
		for i := len(slice); i < to; i = len(slice) {
			slice = append(slice, models.Aggregates{Idx: i})
			slice[i].StartTime, slice[i].EndTime = timesForInterval(i)
		}
		return slice
	}

	// Collect the overall counts and pad the intervals to include empty intervals
	// which are not returned by the db
	aggs.Aggregates = models.Aggregates{StartTime: params.StartTime, EndTime: params.EndTime}
	var (
		bigIntFromStringOK bool
		totalVolume        = big.NewInt(0)
		intervalVolume     = big.NewInt(0)
	)

	// Add each interval, but first pad up to that interval's index
	aggs.Intervals = make([]models.Aggregates, 0, requestedIntervalCount)
	for _, interval := range intervals {
		// Pad up to this interval's position
		aggs.Intervals = padTo(aggs.Intervals, interval.Idx)

		// Format this interval
		interval.StartTime, interval.EndTime = timesForInterval(interval.Idx)

		// Parse volume into a big.Int
		_, bigIntFromStringOK = intervalVolume.SetString(string(interval.TransactionVolume), 10)
		if !bigIntFromStringOK {
			return nil, ErrFailedToParseStringAsBigInt
		}

		// Add to the overall aggregates counts
		totalVolume.Add(totalVolume, intervalVolume)
		aggs.Aggregates.TransactionCount += interval.TransactionCount
		aggs.Aggregates.OutputCount += interval.OutputCount
		aggs.Aggregates.AddressCount += interval.AddressCount
		aggs.Aggregates.AssetCount += interval.AssetCount

		// Add to the list of intervals
		aggs.Intervals = append(aggs.Intervals, interval)
	}
	// Add total aggregated token amounts
	aggs.Aggregates.TransactionVolume = models.TokenAmount(totalVolume.String())

	// Add any missing trailing intervals
	aggs.Intervals = padTo(aggs.Intervals, requestedIntervalCount)

	return aggs, nil
}

func (r *Reader) ListTransactions(ctx context.Context, p *params.ListTransactionsParams) (*models.TransactionList, error) {
	dbRunner := r.conns.DB().NewSession("get_transactions")

	txs := []*models.Transaction{}
	builder := p.Apply(dbRunner.
		Select("avm_transactions.id", "avm_transactions.chain_id", "avm_transactions.type", "avm_transactions.memo", "avm_transactions.created_at").
		From("avm_transactions"))
	if p.NeedsDistinct() {
		builder = builder.Distinct()
	}

	var applySort func(sort params.TransactionSort)
	applySort = func(sort params.TransactionSort) {
		if p.Query != "" {
			return
		}
		switch sort {
		case params.TransactionSortTimestampAsc:
			builder.OrderAsc("avm_transactions.chain_id")
			builder.OrderAsc("avm_transactions.created_at")
		case params.TransactionSortTimestampDesc:
			builder.OrderAsc("avm_transactions.chain_id")
			builder.OrderDesc("avm_transactions.created_at")
		default:
			applySort(params.TransactionSortDefault)
		}
	}
	applySort(p.Sort)

	if _, err := builder.LoadContext(ctx, &txs); err != nil {
		return nil, err
	}

	var count uint64
	if !p.DisableCounting {
		count = uint64(p.Offset) + uint64(len(txs))
		if len(txs) >= p.Limit {
			p.ListParams = params.ListParams{}
			var selector *dbr.SelectStmt
			if p.NeedsDistinct() {
				selector = p.Apply(dbRunner.
					Select("COUNT(DISTINCT(avm_transactions.id))").
					From("avm_transactions"))
			} else {
				selector = p.Apply(dbRunner.
					Select("COUNT(avm_transactions.id)").
					From("avm_transactions"))
			}
			err := selector.
				LoadOneContext(ctx, &count)
			if err != nil {
				return nil, err
			}
		}
	}

	// Add all the addition information we might want
	if err := r.dressTransactions(ctx, dbRunner, txs); err != nil {
		return nil, err
	}

	return &models.TransactionList{ListMetadata: models.ListMetadata{Count: count}, Transactions: txs}, nil
}

func (r *Reader) ListAssets(ctx context.Context, p *params.ListAssetsParams) (*models.AssetList, error) {
	dbRunner := r.conns.DB().NewSession("list_assets")

	assets := []*models.Asset{}
	_, err := p.Apply(dbRunner.
		Select("id", "chain_id", "name", "symbol", "alias", "denomination", "current_supply", "created_at").
		From("avm_assets")).
		LoadContext(ctx, &assets)
	if err != nil {
		return nil, err
	}

	var count uint64
	if !p.DisableCounting {
		count = uint64(p.Offset) + uint64(len(assets))
		if len(assets) >= p.Limit {
			p.ListParams = params.ListParams{}
			err := p.Apply(dbRunner.
				Select("COUNT(avm_assets.id)").
				From("avm_assets")).
				LoadOneContext(ctx, &count)
			if err != nil {
				return nil, err
			}
		}
	}

	return &models.AssetList{ListMetadata: models.ListMetadata{Count: count}, Assets: assets}, nil
}

func (r *Reader) ListAddresses(ctx context.Context, p *params.ListAddressesParams) (*models.AddressList, error) {
	dbRunner := r.conns.DB().NewSession("list_addresses")

	addresses := []*models.AddressInfo{}
	_, err := p.Apply(dbRunner.
		Select("DISTINCT(avm_output_addresses.address)", "addresses.public_key").
		From("avm_output_addresses").
		LeftJoin("addresses", "addresses.address = avm_output_addresses.address")).
		LoadContext(ctx, &addresses)
	if err != nil {
		return nil, err
	}

	var count uint64
	if !p.DisableCounting {
		count = uint64(p.Offset) + uint64(len(addresses))
		if len(addresses) >= p.Limit {
			p.ListParams = params.ListParams{}
			err = p.Apply(dbRunner.
				Select("COUNT(DISTINCT(avm_output_addresses.address))").
				From("avm_output_addresses")).
				LoadOneContext(ctx, &count)
			if err != nil {
				return nil, err
			}
		}
	}

	// Add all the addition information we might want
	if err = r.dressAddresses(ctx, dbRunner, addresses); err != nil {
		return nil, err
	}

	return &models.AddressList{ListMetadata: models.ListMetadata{Count: count}, Addresses: addresses}, nil
}

func (r *Reader) ListOutputs(ctx context.Context, p *params.ListOutputsParams) (*models.OutputList, error) {
	dbRunner := r.conns.DB().NewSession("list_transaction_outputs")

	outputs := []*models.Output{}
	_, err := p.Apply(dbRunner.
		Select(outputSelectColumns...).
		From("avm_outputs")).
		LoadContext(ctx, &outputs)
	if err != nil {
		return nil, err
	}

	if len(outputs) < 1 {
		return &models.OutputList{Outputs: outputs}, nil
	}

	outputIDs := make([]models.StringID, len(outputs))
	outputMap := make(map[models.StringID]*models.Output, len(outputs))
	for i, output := range outputs {
		outputIDs[i] = output.ID
		outputMap[output.ID] = output
	}

	addresses := []*models.OutputAddress{}
	_, err = dbRunner.
		Select(
			"avm_output_addresses.output_id",
			"avm_output_addresses.address",
			"avm_output_addresses.redeeming_signature AS signature",
			"avm_output_addresses.created_at",
		).
		From("avm_output_addresses").
		Where("avm_output_addresses.output_id IN ?", outputIDs).
		LoadContext(ctx, &addresses)
	if err != nil {
		return nil, err
	}

	for _, address := range addresses {
		output := outputMap[address.OutputID]
		if output == nil {
			continue
		}
		output.Addresses = append(output.Addresses, address.Address)
	}

	var count uint64
	if !p.DisableCounting {
		count = uint64(p.Offset) + uint64(len(outputs))
		if len(outputs) >= p.Limit {
			p.ListParams = params.ListParams{}
			err = p.Apply(dbRunner.
				Select("COUNT(avm_outputs.id)").
				From("avm_outputs")).
				LoadOneContext(ctx, &count)
			if err != nil {
				return nil, err
			}
		}
	}

	return &models.OutputList{ListMetadata: models.ListMetadata{Count: count}, Outputs: outputs}, err
}

func (r *Reader) GetTransaction(ctx context.Context, id ids.ID) (*models.Transaction, error) {
	txList, err := r.ListTransactions(ctx, &params.ListTransactionsParams{ID: &id})
	if err != nil {
		return nil, err
	}
	if len(txList.Transactions) > 0 {
		return txList.Transactions[0], nil
	}
	return nil, nil
}

func (r *Reader) GetAsset(ctx context.Context, idStrOrAlias string) (*models.Asset, error) {
	params := &params.ListAssetsParams{}

	id, err := ids.FromString(idStrOrAlias)
	if err == nil {
		params.ID = &id
	} else {
		params.Alias = idStrOrAlias
	}

	assetList, err := r.ListAssets(ctx, params)
	if err != nil {
		return nil, err
	}
	if len(assetList.Assets) > 0 {
		return assetList.Assets[0], nil
	}
	return nil, err
}

func (r *Reader) GetAddress(ctx context.Context, id ids.ShortID) (*models.AddressInfo, error) {
	addressList, err := r.ListAddresses(ctx, &params.ListAddressesParams{Address: &id})
	if err != nil {
		return nil, err
	}
	if len(addressList.Addresses) > 0 {
		return addressList.Addresses[0], nil
	}
	return nil, err
}

func (r *Reader) GetOutput(ctx context.Context, id ids.ID) (*models.Output, error) {
	outputList, err := r.ListOutputs(ctx, &params.ListOutputsParams{ID: &id})
	if err != nil {
		return nil, err
	}
	if len(outputList.Outputs) > 0 {
		return outputList.Outputs[0], nil
	}
	return nil, err
}

func (r *Reader) getFirstTransactionTime(ctx context.Context, chainIDs []string) (time.Time, error) {
	var ts int64
	builder := r.conns.DB().NewSession("get_first_transaction_time").
		Select("COALESCE(UNIX_TIMESTAMP(MIN(created_at)), 0)").
		From("avm_transactions")

	if len(chainIDs) > 0 {
		builder.Where("avm_transactions.chain_id = ?", chainIDs)
	}

	err := builder.LoadOneContext(ctx, &ts)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(ts, 0).UTC(), nil
}

func (r *Reader) dressTransactions(ctx context.Context, dbRunner dbr.SessionRunner, txs []*models.Transaction) error {
	if len(txs) == 0 {
		return nil
	}

	// Get the IDs returned so we can get Input/Output data
	txIDs := make([]models.StringID, len(txs))
	for i, tx := range txs {
		txIDs[i] = tx.ID
	}

	// Load output data for all inputs and outputs into a single list
	// We can't treat them separately because some my be both inputs and outputs
	// for different transactions
	type compositeRecord struct {
		models.Output
		models.OutputAddress
	}

	var outputs []*compositeRecord
	_, err := selectOutputs(dbRunner).
		Where("avm_outputs.transaction_id IN ?", txIDs).
		LoadContext(ctx, &outputs)
	if err != nil {
		return err
	}

	var inputs []*compositeRecord
	_, err = selectOutputs(dbRunner).
		Where("avm_outputs.redeeming_transaction_id IN ?", txIDs).
		LoadContext(ctx, &inputs)
	if err != nil {
		return err
	}

	outputs = append(outputs, inputs...)

	// Create a map of addresses for each output and maps of transaction ids to
	// inputs, outputs, and the total amounts of the inputs and outputs
	var (
		outputAddrs     = make(map[models.StringID]map[models.Address]struct{}, len(txs)*2)
		inputsMap       = make(map[models.StringID]map[models.StringID]*models.Input, len(txs))
		outputsMap      = make(map[models.StringID]map[models.StringID]*models.Output, len(txs))
		inputTotalsMap  = make(map[models.StringID]map[models.StringID]*big.Int, len(txs))
		outputTotalsMap = make(map[models.StringID]map[models.StringID]*big.Int, len(txs))
	)

	// Create a helper to safely add big integers
	addToBigIntMap := func(m map[models.StringID]*big.Int, assetID models.StringID, amt *big.Int) {
		prevAmt := m[assetID]
		if prevAmt == nil {
			prevAmt = big.NewInt(0)
		}
		m[assetID] = prevAmt.Add(amt, prevAmt)
	}

	// Collect outpoints into the maps
	for _, output := range outputs {
		out := &output.Output

		bigAmt := new(big.Int)
		if _, ok := bigAmt.SetString(string(out.Amount), 10); !ok {
			return errors.New("invalid amount")
		}

		if _, ok := inputsMap[out.RedeemingTransactionID]; !ok {
			inputsMap[out.RedeemingTransactionID] = map[models.StringID]*models.Input{}
		}
		if _, ok := inputTotalsMap[out.RedeemingTransactionID]; !ok {
			inputTotalsMap[out.RedeemingTransactionID] = map[models.StringID]*big.Int{}
		}
		if _, ok := outputsMap[out.TransactionID]; !ok {
			outputsMap[out.TransactionID] = map[models.StringID]*models.Output{}
		}
		if _, ok := outputTotalsMap[out.TransactionID]; !ok {
			outputTotalsMap[out.TransactionID] = map[models.StringID]*big.Int{}
		}
		if _, ok := outputAddrs[out.ID]; !ok {
			outputAddrs[out.ID] = map[models.Address]struct{}{}
		}

		outputAddrs[out.ID][output.OutputAddress.Address] = struct{}{}
		outputsMap[out.TransactionID][out.ID] = out
		inputsMap[out.RedeemingTransactionID][out.ID] = &models.Input{Output: out}
		addToBigIntMap(outputTotalsMap[out.TransactionID], out.AssetID, bigAmt)
		addToBigIntMap(inputTotalsMap[out.RedeemingTransactionID], out.AssetID, bigAmt)
	}

	// Collect the addresses into a list on each outpoint
	var input *models.Input
	for _, out := range outputs {
		out.Addresses = make([]models.Address, 0, len(outputAddrs[out.ID]))
		for addr := range outputAddrs[out.ID] {
			out.Addresses = append(out.Addresses, addr)
		}

		// If this Address didn't sign any txs then we're done
		if len(out.Signature) == 0 {
			continue
		}

		// Get the Input and add the credentials for this Address
		for _, input = range inputsMap[out.RedeemingTransactionID] {
			if input.Output.ID.Equals(out.OutputID) {
				input.Creds = append(input.Creds, models.InputCredentials{
					Address:   out.Address,
					PublicKey: out.PublicKey,
					Signature: out.Signature,
				})
				break
			}
		}
	}

	// Add the data we've built up for each transaction
	for _, tx := range txs {
		if inputs, ok := inputsMap[tx.ID]; ok {
			for _, input := range inputs {
				tx.Inputs = append(tx.Inputs, input)
			}
		}

		if outputs, ok := outputsMap[tx.ID]; ok {
			for _, output := range outputs {
				tx.Outputs = append(tx.Outputs, output)
			}
		}

		tx.InputTotals = make(models.AssetTokenCounts, len(inputTotalsMap[tx.ID]))
		for k, v := range inputTotalsMap[tx.ID] {
			tx.InputTotals[k] = models.TokenAmount(v.String())
		}

		tx.OutputTotals = make(models.AssetTokenCounts, len(outputTotalsMap[tx.ID]))
		for k, v := range outputTotalsMap[tx.ID] {
			tx.OutputTotals[k] = models.TokenAmount(v.String())
		}
	}
	return nil
}

func (r *Reader) dressAddresses(ctx context.Context, dbRunner dbr.SessionRunner, addrs []*models.AddressInfo) error {
	if len(addrs) == 0 {
		return nil
	}

	// Create a list of ids for querying, and a map for accumulating results later
	addrIDs := make([]models.Address, len(addrs))
	addrsByID := make(map[models.Address]*models.AddressInfo, len(addrs))
	for i, addr := range addrs {
		addrIDs[i] = addr.Address
		addrsByID[addr.Address] = addr

		addr.Assets = make(map[models.StringID]models.AssetInfo, 1)
	}

	// Load each Transaction Output for the tx, both inputs and outputs
	rows := []*struct {
		Address models.Address `json:"address"`
		models.AssetInfo
	}{}

	_, err := dbRunner.
		Select(
			"avm_output_addresses.address",
			"avm_outputs.asset_id",
			"COUNT(DISTINCT(avm_outputs.transaction_id)) AS transaction_count",
			"COALESCE(SUM(avm_outputs.amount), 0) AS total_received",
			"COALESCE(SUM(CASE WHEN avm_outputs.redeeming_transaction_id != '' THEN avm_outputs.amount ELSE 0 END), 0) AS total_sent",
			"COALESCE(SUM(CASE WHEN avm_outputs.redeeming_transaction_id = '' THEN avm_outputs.amount ELSE 0 END), 0) AS balance",
			"COALESCE(SUM(CASE WHEN avm_outputs.redeeming_transaction_id = '' THEN 1 ELSE 0 END), 0) AS utxo_count",
		).
		From("avm_outputs").
		LeftJoin("avm_output_addresses", "avm_output_addresses.output_id = avm_outputs.id").
		Where("avm_output_addresses.address IN ?", addrIDs).
		GroupBy("avm_output_addresses.address", "avm_outputs.asset_id").
		LoadContext(ctx, &rows)
	if err != nil {
		return err
	}

	// Accumulate rows into addresses
	for _, row := range rows {
		addr, ok := addrsByID[row.Address]
		if !ok {
			continue
		}
		addr.Assets[row.AssetID] = row.AssetInfo
	}

	return nil
}

func (r *Reader) searchByID(ctx context.Context, id ids.ID) (*models.SearchResults, error) {
	listParams := params.ListParams{DisableCounting: true}

	if assets, err := r.ListAssets(ctx, &params.ListAssetsParams{ListParams: listParams, ID: &id}); err != nil {
		return nil, err
	} else if len(assets.Assets) > 0 {
		return collateSearchResults(assets, nil, nil, nil)
	}

	if txs, err := r.ListTransactions(ctx, &params.ListTransactionsParams{ListParams: listParams, ID: &id}); err != nil {
		return nil, err
	} else if len(txs.Transactions) > 0 {
		return collateSearchResults(nil, nil, txs, nil)
	}

	return &models.SearchResults{}, nil
}

func (r *Reader) searchByShortID(ctx context.Context, id ids.ShortID) (*models.SearchResults, error) {
	listParams := params.ListParams{DisableCounting: true}

	if addrs, err := r.ListAddresses(ctx, &params.ListAddressesParams{ListParams: listParams, Address: &id}); err != nil {
		return nil, err
	} else if len(addrs.Addresses) > 0 {
		return collateSearchResults(nil, addrs, nil, nil)
	}

	return &models.SearchResults{}, nil
}

func collateSearchResults(assetResults *models.AssetList, addressResults *models.AddressList, transactionResults *models.TransactionList, _ *models.OutputList) (*models.SearchResults, error) {
	var (
		assets       []*models.Asset
		addresses    []*models.AddressInfo
		transactions []*models.Transaction
		outputs      []*models.Output
	)

	if assetResults != nil {
		assets = assetResults.Assets
	}

	if addressResults != nil {
		addresses = addressResults.Addresses
	}

	if transactionResults != nil {
		transactions = transactionResults.Transactions
	}

	// Build overall SearchResults object from our pieces
	returnedResultCount := len(assets) + len(addresses) + len(transactions) + len(outputs)
	if returnedResultCount > params.PaginationMaxLimit {
		returnedResultCount = params.PaginationMaxLimit
	}

	collatedResults := &models.SearchResults{
		Count: uint64(returnedResultCount),

		// Create a container for our combined results
		Results: make([]models.SearchResult, 0, returnedResultCount),
	}

	// Add each result to the list
	for _, result := range assets {
		collatedResults.Results = append(collatedResults.Results, models.SearchResult{
			SearchResultType: models.ResultTypeAsset,
			Data:             result,
		})
	}
	for _, result := range addresses {
		collatedResults.Results = append(collatedResults.Results, models.SearchResult{
			SearchResultType: models.ResultTypeAddress,
			Data:             result,
		})
	}
	for _, result := range transactions {
		collatedResults.Results = append(collatedResults.Results, models.SearchResult{
			SearchResultType: models.ResultTypeTransaction,
			Data:             result,
		})
	}

	return collatedResults, nil
}

func selectOutputs(dbRunner dbr.SessionRunner) *dbr.SelectBuilder {
	return dbRunner.Select("avm_outputs.id",
		"avm_outputs.transaction_id",
		"avm_outputs.output_index",
		"avm_outputs.asset_id",
		"avm_outputs.output_type",
		"avm_outputs.amount",
		"avm_outputs.locktime",
		"avm_outputs.threshold",
		"avm_outputs.created_at",
		"avm_outputs.redeeming_transaction_id",
		"avm_outputs.group_id",
		"avm_output_addresses.output_id AS output_id",
		"avm_output_addresses.address AS address",
		"avm_output_addresses.redeeming_signature AS signature",
		"addresses.public_key AS public_key",
	).
		From("avm_outputs").
		LeftJoin("avm_output_addresses", "avm_outputs.id = avm_output_addresses.output_id").
		LeftJoin("addresses", "addresses.address = avm_output_addresses.address")
}
