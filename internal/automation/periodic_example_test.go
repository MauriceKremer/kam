package automation

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The sample payload from the real WeatherAPI endpoint, kept verbatim so the
// example script is exercised against the shape it documents.
const weatherPayload = `{
  "location": {
    "name": "Emmen",
    "region": "Drenthe",
    "country": "Netherlands",
    "lat": 52.7792,
    "lon": 6.9069,
    "tz_id": "Europe/Amsterdam",
    "localtime_epoch": 1793991141,
    "localtime": "2026-11-06 19:52"
  },
  "current": {
    "last_updated_epoch": 1793989800,
    "last_updated": "2026-11-06 19:30",
    "temp_c": 4.9,
    "temp_f": 40.9,
    "is_day": 0,
    "condition": {"text": "Clear", "icon": "//cdn.weatherapi.com/weather/64x64/night/113.png", "code": 1000},
    "wind_mph": 8.7,
    "wind_kph": 14.0,
    "wind_degree": 90,
    "wind_dir": "E",
    "pressure_mb": 1025.0,
    "pressure_in": 30.27,
    "precip_mm": 0.0,
    "precip_in": 0.0,
    "humidity": 80,
    "cloud": 0,
    "feelslike_c": 1.8,
    "feelslike_f": 35.2,
    "windchill_c": 1.8,
    "windchill_f": 35.3,
    "heatindex_c": 4.9,
    "heatindex_f": 40.9,
    "dewpoint_c": 1.8,
    "dewpoint_f": 35.3,
    "vis_km": 10.0,
    "vis_miles": 6.0,
    "uv": 0.0,
    "gust_mph": 18.8,
    "gust_kph": 30.3,
    "will_it_rain": 0,
    "chance_of_rain": 10,
    "will_it_snow": 0,
    "chance_of_snow": 0
  }
}`

// periodicSource loads the example and points it at the test server.
func periodicSource(t *testing.T, url string) string {
	t.Helper()
	raw, err := os.ReadFile("../../docs/examples/periodic.star")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	const placeholder = "https://api.weatherapi.com/v1/current.json?key=YOUR_KEY&q=Emmen"
	if !strings.Contains(source, placeholder) {
		t.Fatalf("example no longer contains %s", placeholder)
	}
	return strings.Replace(source, placeholder, url, 1)
}

// The periodic example posts one formatted line per interval, and the line
// matches what the example documents.
func TestPeriodicExamplePostsTheWeatherLine(t *testing.T) {
	fastTimers(t, 40*time.Millisecond)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte(weatherPayload))
	}))
	defer server.Close()

	repository := openTestRepository(t)
	poster := &countPoster{}
	saveTimerScript(t, repository, "weather", periodicSource(t, server.URL))
	newTimerRuntime(t, repository, poster)

	poster.waitForCount(t, 1, 5*time.Second)
	posted := poster.textsAt(0)
	pattern := `^Emmen 4\.9°C, feels 1\.8°C, Clear, wind 14 km/h, humidity 80% \(\d\d:\d\d\)$`
	if !regexp.MustCompile(pattern).MatchString(posted) {
		t.Fatalf("posted %q, want %s", posted, pattern)
	}
}

// A failing API makes the script stay quiet rather than post an error line.
func TestPeriodicExampleStaysQuietOnAFailedRequest(t *testing.T) {
	fastTimers(t, 40*time.Millisecond)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, `{"error":{"code":2009,"message":"Invalid key"}}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	repository := openTestRepository(t)
	poster := &countPoster{}
	saveTimerScript(t, repository, "weather", periodicSource(t, server.URL))
	newTimerRuntime(t, repository, poster)

	time.Sleep(200 * time.Millisecond)
	if posted := poster.count(); posted != 0 {
		t.Fatalf("posted %d times, want none", posted)
	}
}
