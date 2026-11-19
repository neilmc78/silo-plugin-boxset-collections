# Box Set Collections for Silo

A Silo plugin that shows every box set as a collection. Each box set Silo has formed in a movie library gets a matching collection in a **Box Sets** group under that library's Collections. Franchises then appear wherever collections do: the Collections tab, collection rows on Home, and Jellyfin apps.

It reads only the box set data Silo already stores (title, overview, poster, backdrop and movies), so it makes no TMDB requests of its own.

Optionally, with your own free TheTVDB API key, it does the same for TV: each series library gets a **Franchises** group with one collection per official TheTVDB list that holds enough of the library's series, such as [Yellowstone](https://thetvdb.com/lists/yellowstone). See [TV franchises](#tv-franchises).

Series franchise data is provided by [TheTVDB.com](https://thetvdb.com). Please consider adding missing information or subscribing.

## What it does

On each run, for every movie library:

1. Looks up which box set each movie belongs to. One answer covers every movie in that box set, and answers are cached, so routine runs only look up new or refreshed movies.
2. Creates a collection for each box set that forms. Silo forms a box set once the library has enough of its movies and box sets are turned on for the library.
3. Keeps each collection's title, description and movies (in collection order) the same as its box set.
4. Removes a collection once its box set no longer forms, and only when Silo confirms it for that box set. Removals also need a run that looked up every movie successfully, and never happen when a run would remove more than half of a library's box set collections. Those cases are listed in the task's output, and you can delete the collections by hand if the removal was intended.
5. Gives each collection the box set's poster and backdrop, and swaps them when TMDB's artwork changes. If you upload your own image, it is left alone. An image that can't be fetched is tried again on the next run. Silo fetches the image itself when it can. Otherwise the plugin downloads and uploads it, for example when artwork is served from a storage server on your LAN.

A failure on one collection, such as a removal Silo refuses because a Home row still shows the collection, is listed in the task's output and the run moves on to the next collection.

The plugin owns collections with the slug `box-set-<id>` and the group with the slug `box-sets`. It never touches other collections. You can still change visibility, featuring, placement and artwork as usual; only the title, description and movies are overwritten. Don't change a box set collection's slug: the plugin finds its collections by slug, so a renamed one is treated as yours and a new one is created alongside it.

If you ran a Silo build with box set collections built in, those collections use the same slugs. The plugin adopts them instead of making duplicates.

## TV franchises

TV franchises need the **TV Franchises via theTVDB.com** switch on (it is by default) and a TheTVDB API key in **TheTVDB options**. Series libraries need no setting of their own in Silo: unlike movie box sets, which Silo builds and the plugin reads, franchises come straight from TheTVDB. With a key, on each run, for every series library (and mixed library):

1. Reads each series' TheTVDB ID from Silo. Series Silo has no TheTVDB ID for are skipped and counted in the task's output (`series_without_tvdb_id`).
2. Asks TheTVDB which **official** lists each series is on. Lists anyone can make, such as "My favourites", are ignored. Answers are cached: quick runs only look up new or refreshed series, and full runs look a series up again after a week.
3. Creates a collection for each official list that holds at least **Minimum series per franchise** (default 2) of the library's series. It holds those series in the list's order, and only series: films on the same list stay with your movie box sets.
4. Keeps the title, description, series and the list's artwork up to date, and removes a collection once its list no longer qualifies, with the same safeguards as box sets. A run where any TheTVDB lookup fails removes nothing.

Each franchise collection's description ends with `Source: TheTVDB.com (<link to the list>)`. That's the attribution TheTVDB's free API tier requires, shown next to the data viewers see, so leave it in place.

The plugin owns collections with the slug `tvdb-list-<id>` and the group with the slug `franchises`. Changing either group name in the settings renames the group on the next sync.

### Get a TheTVDB key

Every server uses its own key; the plugin doesn't ship one.

1. Sign up on [TheTVDB's API page](https://thetvdb.com/api-information).
2. Choose the free tier (projects under $50k a year, attribution required). Name the project after your server, for example "Silo Box Set Collections on my home server", and describe it as a plugin that groups the series in your own library into franchise collections.
3. In the plugin's **Settings**, leave **TV Franchises via theTVDB.com** on and paste the key in **TheTVDB options** as **TheTVDB API key**. Leave **TheTVDB PIN** empty unless your key is a subscriber key that came with one.
4. Press **Run now** under **Admin → Scheduled Tasks**.

Whether a franchise appears depends on TheTVDB marking its list official. If one you expect is missing, check its page on thetvdb.com.

## Deleting and excluding collections

A collection you delete stays deleted:

- **A TV franchise:** its TheTVDB list ID is added to **Excluded TheTVDB lists** in the plugin's settings. Remove the ID there to bring the collection back on the next sync. You can also add IDs yourself (the number in a list's thetvdb.com address, or shown beside it in the Last sync box) to skip a list before it is ever made.
- **A movie box set:** its TMDB collection ID is added to **Excluded box sets** in the plugin's settings (Box set options). Remove the ID there to bring it back; you can also add IDs yourself (the number in a collection's themoviedb.org address, or shown beside it in the Last sync box).

To keep a collection but hide it from viewers, hide it instead: the plugin never changes a collection's visibility.

If many of a library's collections vanish at once, the plugin assumes something went wrong rather than that you deleted them all, and records nothing that sync.

## Upgrading to 1.1.4 or later

Versions before 1.1.4 used one storage key per collection, and Silo allows a plugin 256 keys with no way to delete one. If the Last sync box says the plugin's storage is full, uninstall and reinstall the plugin once to clear it, then enter the API keys again. Your collections stay; the plugin picks them back up. From 1.1.4 the plugin uses a few dozen keys at most, however large the library.

## When it runs

- **On a schedule, or when you press Run now** under **Admin → Scheduled Tasks**. This is a full run that looks up every movie again. By default the task runs at server start; add a daily schedule there.

  Silo gives a plugin task 10 seconds to answer, so the task starts the sync in the background and answers straight away. Collections appear over the next minute or two.

  Silo doesn't show a plugin task's output, so when the previous sync hit a problem, the next run is marked **failed** and its **Error** column says what happened. For example: `last sync: …; 1 series libraries, 212 series looked up, 0 official lists (38 unofficial, 0 too small), 0 franchises. TheTVDB has no official list for any of your series`. The new sync still starts. With a TheTVDB key set, a sync that forms no franchise counts as a problem too, and the message names the closest lists: for example `Closest: Yellowstone Franchise (7502) matched 1 of your series (list has 6 series; needs 2)`. A run after a clean sync shows as completed. The plugin's page in **Admin → Plugins** also has a **Last sync** box. The plugin writes there when each sync starts and when it finishes, with the full report: movie and series counts, the closest TheTVDB lists that didn't become franchises, and any problems. Reload the page to see the latest. The full summary also goes to the container log (`docker logs`), on lines starting `box set collections:`.
- **After library changes.** About 20 seconds after a library scan or a metadata refresh, a quick run checks new and refreshed movies. A burst of events gives one run.

## Before you start

- **The API key has full admin power.** Silo's API key scopes don't cover collections, so the key must have no scopes, and an unscoped key can do anything its account can. Use the server Owner account, keep the key for this plugin only, and revoke it in **Admin → API Keys** if you uninstall the plugin.
- **The collections show what that account's primary profile can see.** Box sets are worked out per viewer, so if that profile has a rating limit or library restrictions, the movies it can't see are left out of the collections for everyone. Use an account whose primary profile has no limits. The Owner's usually doesn't.
- **Turning box sets on, or lowering the minimum number of movies, shows up on the next full run.** Press Run now after changing either.

## Setup

1. For movie box sets, turn on **Box sets** for your movie libraries in the library settings (Metadata), if they are not on already.
2. In **Admin → API Keys**, create a key with **no scopes** for the server Owner account, or for another administrator. Profile PINs don't matter, because API keys skip them. The Owner account is the best choice: other admins can't revoke or change keys on it.
3. Install the plugin from **Admin → Plugins**, then paste the key under its **Settings → Silo connection**. Turn **Movie box sets** and **TV Franchises via theTVDB.com** on or off as you like; each works without the other, and turning one off hides its options. **Silo URL** is needed only if the plugin can't reach Silo on the address the server reports for itself.
4. Restart Silo, by restarting its container or service. Silo only lists a plugin's scheduled tasks at startup, and the restart also runs the first sync.
5. Under **Admin → Scheduled Tasks**, find **Sync box set collections (neilmc78.collections.boxsets)** in the System category and give it a daily schedule. Silo's own **Sync Movie Box Sets** is a different task: it builds the box set data this plugin reads.

The plugin stays under Silo's rate limit for API keys (10 requests a second, half a standard key's allowance), so a first run on a large library takes a few minutes. Later runs only look up new movies and box sets.

## Building

```bash
make build-all
```

Releases are built by the **Release** workflow, which publishes linux amd64 and arm64 binaries.
