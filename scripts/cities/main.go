package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bapjiws/timezones_mc/datastore/elasticsearch"
	"github.com/bapjiws/timezones_mc/datastore/elasticsearch/configs"
	"github.com/bapjiws/timezones_mc/models/city"
	"github.com/bapjiws/timezones_mc/utils"
	"github.com/satori/go.uuid"
	"gopkg.in/olivere/elastic.v3"
)

var (
	fileFlag        = flag.String("file", "", "file to parse")
	wg              sync.WaitGroup
	start           time.Time
	citiesRead      uint64 = 0
	citiesProcessed uint64 = 0
)

// go run scripts/cities/main.go -file=".raw_data/cities/worldcities.txt"
func main() {
	start = time.Now()

	flag.Parse()

	if *fileFlag == "" {
		fmt.Fprintf(os.Stderr, "CSV file has not been specified. Use the 'file' flag:\n")
		flag.PrintDefaults()
		os.Exit(1)
	}
	fmt.Printf("Importing cities from %s\n", *fileFlag)

	esStore := elasticsearch.NewElasticStore(configs.CityStoreConfig)
	err := esStore.Reindex()
	utils.PanicOnError(err)

	file, err := os.Open(*fileFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error while opening the file: %s\n", err.Error())
		os.Exit(1)
	}
	defer func() {
		if err := file.Close(); err != nil {
			panic(err)
		}
	}()

	csvReader := csv.NewReader(file)
	csvReader.LazyQuotes = true // panic: line 19970, column 7: bare " in non-quoted-field

	headers, err := csvReader.Read()
	utils.PanicOnError(err)
	fmt.Printf("Headers: %v\n", headers) // [Country City AccentCity Region Population Latitude Longitude]

	records := recordGenerator(csvReader)

	// On the usage of bulk processor, see: https://github.com/olivere/elastic/wiki/BulkProcessor
	bulkProcessor, err := esStore.BulkProcessor().
		Workers(runtime.NumCPU()).
		BulkActions(1000).
		Do()
	utils.PanicOnError(err)

	pipe := mergeCityChannels(
		getCityChan(records),
		getCityChan(records),
		getCityChan(records),
		getCityChan(records),
	)
	for city := range pipe {
		atomic.AddUint64(&citiesProcessed, 1)
		bulkRequest := elastic.NewBulkIndexRequest().Index(esStore.IndexName).Type(esStore.TypeName).Id(city.Id).Doc(city)
		bulkProcessor.Add(bulkRequest)
	}

	citiesRead, citiesProcessed := atomic.LoadUint64(&citiesRead), atomic.LoadUint64(&citiesProcessed)
	elapsed := time.Since(start)

	fmt.Printf("Imported %d cities out of %d in %s \n", citiesProcessed, citiesRead, elapsed)

	// Ask workers to commit all requests
	err = bulkProcessor.Flush()
	utils.PanicOnError(err)
}

func recordGenerator(csvReader *csv.Reader) <-chan []string {
	records := make(chan []string)

	go func() {
		for {
			line, err := csvReader.Read()
			if err == io.EOF {
				close(records) // That's it, folks!
				break
			}
			utils.PanicOnError(err)
			records <- line
		}
	}()

	return records
}

func getCityChan(records <-chan []string) chan *city.City {
	cities := make(chan *city.City)

	go func() {
		for record := range records {
			id := uuid.NewV4().String()
			latitude, _ := strconv.ParseFloat(record[5], 64)
			longitude, _ := strconv.ParseFloat(record[6], 64)

			city := &city.City{
				Id:          id,
				Name:        record[1], // TODO: All names are lowercase -- do something about it?
				AccentName:  record[2],
				CountryCode: record[0],
				Latitude:    latitude,
				Longitude:   longitude,
				Suggest: elastic.NewSuggestField().
					Input(record[1]).
					Output(record[2]).
					Payload(map[string]string{"city_id": id}),
			}

			atomic.AddUint64(&citiesRead, 1)
			cities <- city
		}

		close(cities)
	}()

	return cities

}

func mergeCityChannels(cityChannels ...chan *city.City) chan *city.City {
	pipe := make(chan *city.City)

	output := func(cityChan <-chan *city.City) {
		for city := range cityChan {
			pipe <- city
		}
		wg.Done()
	}

	wg.Add(len(cityChannels))
	for _, cityChan := range cityChannels {
		go output(cityChan)
	}

	go func() {
		wg.Wait()
		close(pipe)
	}()

	return pipe
}
