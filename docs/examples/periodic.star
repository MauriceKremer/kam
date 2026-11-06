# PERIODIC weather post: every 15 minutes read the current weather and put one
# line into a room. Edit the settings below, then save the script as described
# in the README ("Automation (Starlark scripts)").
#
# Save it with timeout_ms = 30000: the 500 ms default is fine for a reply on
# air, but a cold HTTPS round trip to an API can take longer than that.
timer_interval_minutes = 15

ROOM = "lobby"
API_URL = "https://api.weatherapi.com/v1/current.json?key=YOUR_KEY&q=Emmen"

def on_timer(message):
    body, status = rest_get(API_URL)
    if status != 200:
        return  # stay quiet and try again next interval; a bad key is not room news
    weather = json.decode(body)
    current = weather["current"]
    text = "Emmen %s°C, feels %s°C, %s, wind %d km/h, humidity %d%% (%s)" % (
        current["temp_c"],
        current["feelslike_c"],
        current["condition"]["text"],
        current["wind_kph"],
        current["humidity"],
        time.from_timestamp(message["timestamp_unix"]).format("15:04"),
    )
    room_post(ROOM, text)
