# Nightcache

Nightcache is a small web app that imports a public Spotify playlist, shows the real tracks, and can queue an audio download for each track.

The app is deliberately split into one web service: the same server serves the HTML UI and the API, so there is no CORS setup and no Windows `.exe` needed for the hosted version.

## What you need

- A GitHub account.
- A Render account.
- A public Spotify playlist URL.

## The easiest way to put it online

### 1. Create a GitHub repository

Create a new repository called `Nightcache` (Public is easiest for a first test).
Do not create a README during repository creation.

Upload these files from this folder:

```text
Dockerfile
.dockerignore
.gitignore
go.mod
main.go
index.html
render.yaml
README.md
```

You do NOT need `Nightcache.exe` for the web version.

### 2. Create the live site on Render

Open Render and choose **New → Web Service**.
Connect your GitHub account and select the `Nightcache` repository.

Use:

- Environment / Language: **Docker**
- Branch: `main`
- Plan: **Free** for testing
- Health check path: `/api/health`

Render builds the Dockerfile, installs FFmpeg, yt-dlp and Deno, then starts Nightcache. Each web service gets a public `onrender.com` address.

### 3. Open your site

When the deploy finishes, Render shows a URL similar to:

```text
https://nightcache-xxxx.onrender.com
```

Open it in Chrome. You no longer need to open a folder or launch `Nightcache.exe`.

## Updating the site

Whenever you change a file in GitHub and commit it to `main`, Render can rebuild and redeploy the service automatically.

## Important limits for this prototype

- Spotify import is based on the public Spotify embed page, so a playlist must be public.
- The server intentionally downloads one track at a time to keep a small instance from being overwhelmed.
- Finished temporary MP3 files expire after about 45 minutes and are removed.
- A free Render web service can sleep after a period of inactivity, so the first request after idle time can be slower.
- A 100-track playlist can take a long time. For a first public test, try 3–10 tracks.

## About downloads

The download flow uses `yt-dlp` plus FFmpeg and Deno. Use the feature only for audio you are allowed to download and convert. Check the terms of the source platform and the rights for the specific recording.
