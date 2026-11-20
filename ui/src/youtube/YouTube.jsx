import React, { useCallback, useEffect, useRef, useState } from 'react'
import {
  Button,
  CircularProgress,
  LinearProgress,
  Paper,
  TextField,
  Typography,
  makeStyles,
} from '@material-ui/core'
import RefreshIcon from '@material-ui/icons/Refresh'
import DeleteOutlineIcon from '@material-ui/icons/DeleteOutline'
import { baseUrl } from '../utils'

// Playlist snapshots are read from /music/users/<username>/musicasyt.json
// through Musicas' authenticated Go API. No browser storage is required and
// loading this page never contacts YouTube or starts a worker job.
const useStyles = makeStyles((theme) => ({
  root: { maxWidth: 1000, padding: theme.spacing(3), margin: 'auto' },
  header: { marginBottom: theme.spacing(2) },
  controls: { display: 'flex', alignItems: 'flex-start', gap: theme.spacing(1), flexWrap: 'wrap', marginBottom: theme.spacing(3) },
  url: { flex: '1 1 370px' },
  item: { padding: theme.spacing(2), marginBottom: theme.spacing(2) },
  row: { display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: theme.spacing(1), justifyContent: 'space-between' },
  actions: { display: 'flex', gap: theme.spacing(1), alignItems: 'center' },
  title: { wordBreak: 'break-word', fontWeight: 600 },
  detail: { wordBreak: 'break-all', marginTop: theme.spacing(1) },
  stats: { display: 'flex', flexWrap: 'wrap', gap: theme.spacing(2), marginTop: theme.spacing(1), marginBottom: theme.spacing(1) },
  progress: { marginTop: theme.spacing(1) },
  notice: { marginTop: theme.spacing(1), marginBottom: theme.spacing(2), whiteSpace: 'pre-wrap' },
  trackIssues: { marginTop: theme.spacing(1), paddingTop: theme.spacing(1), borderTop: `1px solid ${theme.palette.divider}` },
  issueList: { margin: theme.spacing(0.5, 0, 0), paddingLeft: theme.spacing(2.5) },
  issueLink: { color: theme.palette.primary.main, overflowWrap: 'anywhere' },
}))

const apiURL = (endpoint) => baseUrl(`api/youtube/${endpoint}`)
const request = async (endpoint, options = {}) => {
  const response = await fetch(apiURL(endpoint), {
    credentials: 'same-origin',
    headers: {
      'X-ND-Authorization': `Bearer ${localStorage.getItem('token') || ''}`,
      ...(options.body ? { 'Content-Type': 'application/json' } : {}),
    },
    ...options,
  })
  const body = await response.json().catch(() => ({}))
  if (!response.ok) {
    const error = new Error(body.error || body.detail || `Request failed: HTTP ${response.status}`)
    error.status = response.status
    throw error
  }
  return body
}

const parsePlaylist = (input) => {
  let url
  try { url = new URL(input.trim()) } catch (_) { throw new Error('Enter a valid YouTube Music playlist URL.') }
  if (url.protocol !== 'https:' || !['youtube.com', 'www.youtube.com', 'music.youtube.com', 'm.youtube.com'].includes(url.hostname) || (url.port && url.port !== '443')) {
    throw new Error('Only HTTPS YouTube playlist links are supported.')
  }
  const ids = url.searchParams.getAll('list')
  if (ids.length !== 1 || !/^[A-Za-z0-9_-]{5,128}$/.test(ids[0])) {
    throw new Error('The link must contain a valid playlist ID.')
  }
  return { playlist_id: ids[0], playlist_url: url.toString(), title: ids[0], status: 'queued' }
}

const activeStatuses = new Set(['queued', 'reading_playlist', 'checking_files', 'downloading'])
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
const labelStatus = (status) => ({
  queued: 'Queued', reading_playlist: 'Reading YouTube playlist', checking_files: 'Checking downloaded files',
  downloading: 'Downloading', completed: 'Completed', completed_with_errors: 'Completed with errors',
  error: 'Failed', previously_synced: 'Previously downloaded',
}[status] || status || 'Not synchronized')
const localDate = (value) => value && !Number.isNaN(Date.parse(value)) ? new Date(value).toLocaleString() : ''

export default function YouTube() {
  const classes = useStyles()
  const [url, setURL] = useState('')
  const [playlists, setPlaylists] = useState([])
  const [optimistic, setOptimistic] = useState({})
  const [pending, setPending] = useState({})
  const [errors, setErrors] = useState({})
  const [worker, setWorker] = useState(null)
  const [checkingWorker, setCheckingWorker] = useState(false)
  const mountedRef = useRef(false)
  const pendingRef = useRef(new Set())

  const reload = useCallback(async () => {
    try {
      const result = await request('playlists')
      if (mountedRef.current) {
        setPlaylists(Array.isArray(result.playlists) ? result.playlists : [])
        setOptimistic((old) => {
          const next = { ...old }
          for (const record of (result.playlists || [])) delete next[record.playlist_id]
          return next
        })
      }
    } catch (error) {
      if (mountedRef.current) setErrors((old) => ({ ...old, list: error.message }))
    }
  }, [])

  useEffect(() => {
    mountedRef.current = true
    reload()
    // Local file snapshot only. No worker wake-up, YouTube API call, or sync.
    const timer = setInterval(reload, 5000)
    return () => { mountedRef.current = false; clearInterval(timer) }
  }, [reload])

  const checkWorker = useCallback(async () => {
    setCheckingWorker(true)
    try {
      const info = await request('status')
      if (mountedRef.current) setWorker(info)
      return info
    } catch (error) {
      if (mountedRef.current) setWorker({ ready: false, state: 'unavailable', message: error.message })
      return null
    } finally {
      if (mountedRef.current) setCheckingWorker(false)
    }
  }, [])

  const startSync = useCallback(async (entry) => {
    const id = entry.playlist_id
    if (pendingRef.current.has(id)) return
    pendingRef.current.add(id)
    setPending((old) => ({ ...old, [id]: true }))
    setOptimistic((old) => ({ ...old, [id]: entry }))
    setErrors((old) => ({ ...old, [id]: '' }))
    const start = Date.now()
    try {
      while (mountedRef.current) {
        try {
          const job = await request('sync', {
            method: 'POST', body: JSON.stringify({ playlist_url: entry.playlist_url, restart: true }),
          })
          if (mountedRef.current) {
            setWorker((old) => ({ ...old, ready: true, state: 'ready', message: 'Worker available' }))
            setOptimistic((old) => ({ ...old, [id]: { ...old[id], ...job } }))
            await reload()
          }
          return
        } catch (error) {
          if (![502, 503, 504].includes(error.status) || Date.now() - start > 15 * 60 * 1000) throw error
          if (mountedRef.current) {
            setWorker((old) => ({ ...old, ready: false, state: 'starting', message: 'Waking worker or updating Python dependencies...' }))
            await checkWorker()
          }
          await sleep(8000)
        }
      }
    } catch (error) {
      if (mountedRef.current) setErrors((old) => ({ ...old, [id]: error.message }))
    } finally {
      pendingRef.current.delete(id)
      if (mountedRef.current) setPending((old) => ({ ...old, [id]: false }))
    }
  }, [checkWorker, reload])

  const submit = async (event) => {
    event.preventDefault()
    try {
      const entry = parsePlaylist(url)
      setURL('')
      await startSync(entry)
    } catch (error) {
      setErrors((old) => ({ ...old, form: error.message }))
    }
  }

  const remove = async (entry) => {
    if (pending[entry.playlist_id]) return
    if (!window.confirm(`Permanently delete "${entry.title || entry.playlist_id}" and its downloaded folder, including all songs?`)) return
    setPending((old) => ({ ...old, [entry.playlist_id]: true }))
    try {
      await request(`playlists/${encodeURIComponent(entry.playlist_id)}`, { method: 'DELETE' })
      setOptimistic((old) => { const next = { ...old }; delete next[entry.playlist_id]; return next })
      setPlaylists((old) => old.filter((item) => item.playlist_id !== entry.playlist_id))
      await reload()
    } catch (error) {
      if (mountedRef.current) setErrors((old) => ({ ...old, [entry.playlist_id]: error.message }))
    } finally {
      if (mountedRef.current) setPending((old) => ({ ...old, [entry.playlist_id]: false }))
    }
  }

  const merged = [...playlists, ...Object.values(optimistic).filter(
    (entry) => !playlists.some((p) => p.playlist_id === entry.playlist_id),
  )]
  return (
    <div className={classes.root}>
      <Typography variant="h5" className={classes.header}>YouTube</Typography>
      <Typography color="textSecondary" className={classes.header}>
        Manually synchronize YouTube Music playlists into your private library. The last known status is stored with your music.
      </Typography>
      <form onSubmit={submit} className={classes.controls}>
        <TextField className={classes.url} label="YouTube Music playlist URL" variant="outlined"
          size="small" value={url} onChange={(event) => { setURL(event.target.value); setErrors((old) => ({ ...old, form: '' })) }}
          placeholder="https://music.youtube.com/playlist?list=..." />
        <Button type="submit" variant="contained" color="primary" disabled={!url.trim()}>Sync playlist</Button>
        <Button variant="outlined" type="button" onClick={checkWorker} disabled={checkingWorker}>
          {checkingWorker ? <CircularProgress size={18} /> : 'Worker status'}
        </Button>
      </form>
      {errors.form && <Typography color="error" className={classes.notice}>{errors.form}</Typography>}
      {errors.list && <Typography color="error" className={classes.notice}>{errors.list}</Typography>}
      {worker && <Paper className={classes.item} elevation={1}>
        <Typography variant="subtitle2">Worker: {worker.state || 'unknown'} {worker.ready ? '- Ready' : ''}</Typography>
        <Typography variant="body2" color="textSecondary">{worker.message}</Typography>
        {worker.commits && <Typography variant="caption" color="textSecondary">
          ytmusicapi: {worker.commits.ytmusicapi?.slice(0, 12) || '-'} · yt-dlp: {worker.commits['yt-dlp']?.slice(0, 12) || '-'}
        </Typography>}
      </Paper>}
      <Typography variant="h6" className={classes.header}>My playlists</Typography>
      {!merged.length && <Typography color="textSecondary">Paste a playlist URL and press Sync to add it here.</Typography>}
      {merged.map((entry) => {
        const id = entry.playlist_id
        const stale = activeStatuses.has(entry.status) && entry.updated_at && Date.now() - Date.parse(entry.updated_at) > 90000
        const unavailableTracks = Object.entries(entry.tracks_unavailable || {}).filter(([videoID]) => /^[A-Za-z0-9_-]{5,128}$/.test(videoID))
        const failedTracks = Object.entries(entry.tracks_error || {}).filter(([videoID]) => /^[A-Za-z0-9_-]{5,128}$/.test(videoID))
        const renderTrackIssues = (label, tracks) => tracks.length > 0 && (
          <div className={classes.trackIssues}>
            <Typography variant="subtitle2">{label} ({tracks.length})</Typography>
            <ul className={classes.issueList}>
              {tracks.map(([videoID, filename]) => (
                <li key={videoID}>
                  <a className={classes.issueLink} href={`https://music.youtube.com/watch?v=${encodeURIComponent(videoID)}`} target="_blank" rel="noopener noreferrer">
                    {filename || videoID}
                  </a>
                </li>
              ))}
            </ul>
          </div>
        )
        return <Paper key={id} className={classes.item} elevation={2}>
          <div className={classes.row}>
            <div style={{ flex: '1 1 300px', minWidth: 0 }}>
              <Typography className={classes.title}>{entry.title || id}</Typography>
              <Typography variant="body2" color="textSecondary">
                {labelStatus(entry.status)}{stale ? ' (last recorded; worker may have restarted)' : ''}
              </Typography>
              {entry.folder && <Typography variant="caption" className={classes.detail} color="textSecondary" display="block">{entry.folder}</Typography>}
              {!entry.folder && entry.folder_name && <Typography variant="caption" color="textSecondary" display="block">{entry.folder_name}</Typography>}
              {entry.last_synced_at && <Typography variant="caption" display="block" color="textSecondary">Last sync: {localDate(entry.last_synced_at)}</Typography>}
              {entry.updated_at && <Typography variant="caption" display="block" color="textSecondary">Updated: {localDate(entry.updated_at)}</Typography>}
            </div>
            <div className={classes.actions}>
              <Button size="small" variant="outlined" color="primary" startIcon={pending[id] ? <CircularProgress size={14} /> : <RefreshIcon />}
                disabled={Boolean(pending[id])} onClick={() => startSync(entry)}>
                {pending[id] ? 'Connecting...' : 'Sync'}
              </Button>
              <Button size="small" aria-label="Delete playlist and downloaded folder" disabled={Boolean(pending[id])}
                onClick={() => remove(entry)}><DeleteOutlineIcon /></Button>
            </div>
          </div>
          {typeof entry.percent === 'number' && <LinearProgress className={classes.progress} variant="determinate" value={Math.min(100, Math.max(0, entry.percent))} />}
          {entry.total != null && <div className={classes.stats}>
            <Typography variant="body2">Total: {entry.total}</Typography>
            <Typography variant="body2">New: {entry.downloaded ?? 0}</Typography>
            <Typography variant="body2">Existing: {entry.existing ?? 0}</Typography>
            <Typography variant="body2">Failed: {entry.failed ?? 0}</Typography>
            <Typography variant="body2">Unavailable: {entry.unavailable ?? 0}</Typography>
            <Typography variant="body2">Remaining: {entry.remaining ?? '-'}</Typography>
          </div>}
          {entry.current_track && <Typography variant="body2" color="textSecondary">Last track: {entry.current_track}</Typography>}
          {renderTrackIssues('Unavailable tracks', unavailableTracks)}
          {renderTrackIssues('Failed tracks', failedTracks)}
          {entry.error && <Typography color="error" variant="body2">{entry.error}</Typography>}
          {pending[id] && <Typography variant="caption" display="block" color="textSecondary">Contacting MusicasYT; it may be waking up...</Typography>}
          {errors[id] && <Typography color="error" variant="body2">{errors[id]}</Typography>}
        </Paper>
      })}
      <Typography variant="caption" color="textSecondary">
        Playlist information is stored in your private music folder. Jobs are in memory; a worker restart can interrupt a sync, but downloading never resumes automatically.
      </Typography>
    </div>
  )
}
