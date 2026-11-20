import React, { useCallback, useEffect, useRef, useState } from 'react'
import { useDispatch } from 'react-redux'
import {
  Button,
  Chip,
  CircularProgress,
  Divider,
  LinearProgress,
  List,
  ListItem,
  ListItemText,
  Paper,
  Radio,
  RadioGroup,
  FormControlLabel,
  TextField,
  Typography,
  makeStyles,
} from '@material-ui/core'
import MusicNoteIcon from '@material-ui/icons/MusicNote'
import SearchIcon from '@material-ui/icons/Search'
import RefreshIcon from '@material-ui/icons/Refresh'
import SaveIcon from '@material-ui/icons/Save'
import { setLyrics } from '../actions'
import { baseUrl } from '../utils'

const useStyles = makeStyles((theme) => ({
  page: { maxWidth: 1050, padding: theme.spacing(3), margin: 'auto' },
  header: { marginBottom: theme.spacing(2) },
  section: { padding: theme.spacing(2), marginBottom: theme.spacing(2) },
  toolbar: { display: 'flex', alignItems: 'center', gap: theme.spacing(1), flexWrap: 'wrap', marginTop: theme.spacing(1) },
  folderRow: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: theme.spacing(2), padding: theme.spacing(1, 0) },
  folderName: { minWidth: 0, overflowWrap: 'anywhere' },
  searchField: { flex: '1 1 320px' },
  song: { borderBottom: `1px solid ${theme.palette.divider}` },
  result: { padding: theme.spacing(2), marginTop: theme.spacing(1), border: `1px solid ${theme.palette.divider}` },
  selected: { borderColor: theme.palette.primary.main, background: theme.palette.action.hover },
  meta: { marginTop: theme.spacing(0.5), overflowWrap: 'anywhere' },
  preview: {
    margin: theme.spacing(1, 0), padding: theme.spacing(1.5),
    background: theme.palette.background.default, borderRadius: 4,
    whiteSpace: 'pre-wrap', overflowWrap: 'anywhere', maxHeight: 280, overflow: 'auto',
    fontFamily: 'monospace', fontSize: 13,
  },
  message: { margin: theme.spacing(1, 0), whiteSpace: 'pre-wrap' },
  muted: { color: theme.palette.text.secondary },
  actions: { display: 'flex', gap: theme.spacing(1), alignItems: 'center', flexWrap: 'wrap' },
  scanProgress: { marginTop: theme.spacing(1), maxWidth: 520 },
}))

const apiURL = (suffix = '') => baseUrl(`api/lyrics/${suffix}`)
const request = async (suffix, options = {}) => {
  const response = await fetch(apiURL(suffix), {
    credentials: 'same-origin',
    headers: {
      'X-ND-Authorization': `Bearer ${localStorage.getItem('token') || ''}`,
      ...(options.body ? { 'Content-Type': 'application/json' } : {}),
    },
    ...options,
  })
  const body = await response.json().catch(() => ({}))
  if (!response.ok) {
    const error = new Error(body.error || `Request failed: HTTP ${response.status}`)
    error.status = response.status
    throw error
  }
  return body
}

const durationLabel = (seconds) => {
  if (!Number.isFinite(Number(seconds)) || Number(seconds) <= 0) return 'Unknown duration'
  const total = Math.round(Number(seconds))
  return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, '0')}`
}

export default function Lyrics() {
  const classes = useStyles()
  const dispatch = useDispatch()
  const mountedRef = useRef(false)
  const [status, setStatus] = useState({ scanning: false, available: false, progressAvailable: false })
  const [folders, setFolders] = useState([])
  const [loadingFolders, setLoadingFolders] = useState(true)
  const [scanPending, setScanPending] = useState(false)

  const [lyricsQuery, setLyricsQuery] = useState('')
  const [lyricsResults, setLyricsResults] = useState([])
  const [lyricsSearched, setLyricsSearched] = useState(false)
  const [searchingLyrics, setSearchingLyrics] = useState(false)
  const [selectedResult, setSelectedResult] = useState(null)
  const [selectedFormat, setSelectedFormat] = useState('synced')

  const [libraryQuery, setLibraryQuery] = useState('')
  const [librarySongs, setLibrarySongs] = useState([])
  const [librarySearched, setLibrarySearched] = useState(false)
  const [searchingLibrary, setSearchingLibrary] = useState(false)
  const [selectedSong, setSelectedSong] = useState(null)

  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const [message, setMessage] = useState('')

  const refreshStatus = useCallback(async () => {
    try {
      const result = await request('status')
      if (mountedRef.current) setStatus(result)
    } catch (err) {
      if (mountedRef.current) {
        setStatus({ scanning: false, available: false, progressAvailable: false })
        setError(err.message)
      }
    }
  }, [])

  const loadFolders = useCallback(async () => {
    setLoadingFolders(true)
    try {
      const result = await request('folders')
      if (mountedRef.current) setFolders(Array.isArray(result.folders) ? result.folders : [])
    } catch (err) {
      if (mountedRef.current) setError(err.message)
    } finally {
      if (mountedRef.current) setLoadingFolders(false)
    }
  }, [])

  useEffect(() => {
    mountedRef.current = true
    refreshStatus()
    loadFolders()
    const timer = setInterval(refreshStatus, 3000)
    return () => {
      mountedRef.current = false
      clearInterval(timer)
    }
  }, [loadFolders, refreshStatus])

  const startScan = async (folder = '') => {
    if (scanPending || status.scanning || !status.available) return
    setScanPending(true)
    setError('')
    setMessage('')
    try {
      await request('scan', { method: 'POST', body: JSON.stringify(folder ? { folder } : {}) })
      setMessage(folder ? `Lyrics scan started for ${folder}.` : 'Lyrics scan started for all your folders.')
      await refreshStatus()
    } catch (err) {
      setError(err.message)
      await refreshStatus()
    } finally {
      if (mountedRef.current) setScanPending(false)
    }
  }

  const searchLyrics = async (event) => {
    if (event) event.preventDefault()
    const query = lyricsQuery.trim()
    if (query.length < 2) {
      setError('Enter at least two characters to search LRCLIB.')
      return
    }
    setSearchingLyrics(true)
    setError('')
    setMessage('')
    setSelectedResult(null)
    setLyricsResults([])
    setLyricsSearched(false)
    setLibraryQuery('')
    setLibrarySongs([])
    setLibrarySearched(false)
    setSelectedSong(null)
    try {
      const result = await request(`search?q=${encodeURIComponent(query)}`)
      if (mountedRef.current) {
        setLyricsResults(Array.isArray(result.results) ? result.results : [])
        setLyricsSearched(true)
      }
    } catch (err) {
      if (mountedRef.current) setError(err.message)
    } finally {
      if (mountedRef.current) setSearchingLyrics(false)
    }
  }

  const searchLibrary = async (event) => {
    if (event) event.preventDefault()
    const query = libraryQuery.trim()
    if (query.length < 2) {
      setError('Enter at least two characters to search your private library.')
      return
    }
    setSearchingLibrary(true)
    setError('')
    setLibrarySearched(false)
    setLibrarySongs([])
    setSelectedSong(null)
    try {
      const result = await request(`songs?q=${encodeURIComponent(query)}`)
      if (mountedRef.current) {
        setLibrarySongs(Array.isArray(result.songs) ? result.songs : [])
        setLibrarySearched(true)
      }
    } catch (err) {
      if (mountedRef.current) setError(err.message)
    } finally {
      if (mountedRef.current) setSearchingLibrary(false)
    }
  }

  const selectResult = (result) => {
    const isClosing = selectedResult?.id === result.id
    setSelectedResult(isClosing ? null : result)
    if (!isClosing) setSelectedFormat(result.syncedLyrics ? 'synced' : 'plain')
    setLibraryQuery('')
    setLibrarySongs([])
    setLibrarySearched(false)
    setSelectedSong(null)
  }

  const applyLyrics = async () => {
    if (!selectedSong || !selectedResult || saving) return
    setSaving(true)
    setError('')
    setMessage('')
    try {
      await request('apply', {
        method: 'POST',
        body: JSON.stringify({
          songId: selectedSong.id,
          query: lyricsQuery.trim(),
          resultId: selectedResult.id,
          format: selectedFormat,
        }),
      })
      dispatch(setLyrics(selectedSong.id, preview))
      setMessage(`Lyrics saved beside ${selectedSong.title}.`)
    } catch (err) {
      setError(err.message)
    } finally {
      if (mountedRef.current) setSaving(false)
    }
  }

  const preview = selectedResult
    ? (selectedFormat === 'synced' ? selectedResult.syncedLyrics : selectedResult.plainLyrics)
    : ''

  return (
    <div className={classes.page}>
      <Typography variant="h5" className={classes.header}>Lyrics</Typography>
      <Typography color="textSecondary" className={classes.header}>
        Find and save lyrics in your private music library. Automatic scans skip songs that already have lyrics.
      </Typography>

      {error && <Typography color="error" className={classes.message}>{error}</Typography>}
      {message && <Typography color="primary" className={classes.message}>{message}</Typography>}

      <Paper className={classes.section} elevation={1}>
        <div className={classes.folderRow}>
          <div>
            <Typography variant="subtitle1">Scan status</Typography>
            <Typography variant="body2" className={classes.muted}>
              {status.scanning ? 'A lyrics scan is running.' : status.available ? 'A lyrics scan can be started.' : 'Lyrics status is unavailable.'}
            </Typography>
            {status.scanning && status.progressAvailable && (
              <>
                <Typography variant="caption" color="textSecondary">
                  {`${status.processed} of ${status.total} songs processed · ${status.remaining} remaining`}
                </Typography>
                <LinearProgress className={classes.scanProgress} variant="determinate" value={Math.min(100, Math.max(0, status.percent || 0))} />
              </>
            )}
          </div>
          <Chip
            label={status.scanning ? 'Scan running' : status.available ? 'Available' : 'Unavailable'}
            color={status.scanning ? 'default' : status.available ? 'primary' : 'secondary'}
            variant={status.scanning || !status.available ? 'outlined' : 'default'}
          />
        </div>
        <Divider />
        <div className={classes.toolbar}>
          <Button variant="contained" color="primary" startIcon={scanPending ? <CircularProgress size={16} /> : <RefreshIcon />}
            disabled={scanPending || status.scanning || !status.available} onClick={() => startScan()}>
            {scanPending ? 'Starting scan…' : 'Scan all folders'}
          </Button>
          <Button variant="outlined" onClick={refreshStatus}>Refresh status</Button>
        </div>
      </Paper>

      <Paper className={classes.section} elevation={1}>
        <Typography variant="h6">Library folders</Typography>
        <Typography variant="body2" color="textSecondary">
          Each scan includes all subfolders beneath the selected folder.
        </Typography>
        {loadingFolders ? <CircularProgress size={22} /> : folders.length === 0 ? (
          <Typography variant="body2" color="textSecondary">No folders were found in your private library.</Typography>
        ) : (
          <List disablePadding>
            {folders.map((folder) => (
              <React.Fragment key={folder.path}>
                <div className={classes.folderRow}>
                  <Typography className={classes.folderName}>{folder.name}</Typography>
                  <Button size="small" variant="outlined" color="primary"
                    disabled={scanPending || status.scanning || !status.available} onClick={() => startScan(folder.path)}>
                    Scan
                  </Button>
                </div>
                <Divider />
              </React.Fragment>
            ))}
          </List>
        )}
      </Paper>

      <Paper className={classes.section} elevation={1}>
        <Typography variant="h6">Manual lyrics search</Typography>
        <Typography variant="body2" color="textSecondary">
          Search LRCLIB for any song or artist. Preview the lyrics first, then optionally save them beside a song in your private library.
        </Typography>
        <form className={classes.toolbar} onSubmit={searchLyrics}>
          <TextField className={classes.searchField} size="small" variant="outlined" label="Song title, artist or both"
            placeholder="e.g. Charlie Brown Jr. Ela vai voltar"
            value={lyricsQuery} onChange={(event) => {
              setLyricsQuery(event.target.value)
              setLyricsSearched(false)
              setLyricsResults([])
              setSelectedResult(null)
            }} />
          <Button type="submit" variant="contained" color="primary" startIcon={searchingLyrics ? <CircularProgress size={16} /> : <SearchIcon />}
            disabled={searchingLyrics}>
            Search LRCLIB
          </Button>
        </form>

        {searchingLyrics ? <CircularProgress size={22} /> : lyricsSearched && lyricsResults.length === 0 ? (
          <Typography variant="body2" color="textSecondary">No usable lyrics results found. Try another song title or artist.</Typography>
        ) : (
          (selectedResult ? lyricsResults.filter((result) => result.id === selectedResult.id) : lyricsResults).map((result) => {
            const selected = selectedResult?.id === result.id
            const formats = [result.syncedLyrics ? 'Synced (.lrc)' : '', result.plainLyrics ? 'Plain (.txt)' : ''].filter(Boolean).join(' · ')
            return (
              <Paper key={result.id} elevation={selected ? 2 : 0}
                className={`${classes.result} ${selected ? classes.selected : ''}`}>
                <div className={classes.folderRow}>
                  <div style={{ minWidth: 0 }}>
                    <Typography variant="subtitle1">{result.trackName || 'Untitled'}</Typography>
                    <Typography variant="body2" color="textSecondary">
                      {result.artistName || 'Unknown artist'}{result.albumName ? ` · ${result.albumName}` : ''} · {durationLabel(result.duration)}
                    </Typography>
                    <Typography variant="caption" color="textSecondary">Available: {formats}</Typography>
                  </div>
                  <Button size="small" variant={selected ? 'contained' : 'outlined'} color="primary"
                    aria-expanded={selected} onClick={() => selectResult(result)}>
                    {selected ? 'Stop Preview' : 'Preview'}
                  </Button>
                </div>
                {selected && (
                  <div>
                    <Divider style={{ margin: '16px 0' }} />
                    <Typography variant="subtitle1">Lyrics preview</Typography>
                    <Typography variant="body2" color="textSecondary">
                      {selectedResult.trackName} - {selectedResult.artistName} · {selectedResult.albumName || 'Unknown album'} · {durationLabel(selectedResult.duration)}
                    </Typography>
                    <RadioGroup row value={selectedFormat} onChange={(event) => setSelectedFormat(event.target.value)}>
                      {selectedResult.syncedLyrics && <FormControlLabel value="synced" control={<Radio color="primary" />} label="Synced (.lrc)" />}
                      {selectedResult.plainLyrics && <FormControlLabel value="plain" control={<Radio color="primary" />} label="Plain (.txt)" />}
                    </RadioGroup>
                    <div className={classes.preview}>{preview || 'No lyrics available in this format.'}</div>

                    <Divider style={{ margin: '16px 0' }} />
                    <Typography variant="subtitle1">Save lyrics to a song in your library</Typography>
                    <Typography variant="body2" color="textSecondary">
                      Choose the destination track. The lyrics will be saved in the same folder with the track's filename, using the selected lyrics format. Existing sidecars will be replaced.
                    </Typography>
                    <form className={classes.toolbar} onSubmit={searchLibrary}>
                      <TextField className={classes.searchField} size="small" variant="outlined" label="Find a song in your private library"
                        placeholder="Search by title, artist, album or filename"
                        value={libraryQuery} onChange={(event) => {
                        setLibraryQuery(event.target.value)
                        setLibrarySearched(false)
                        setLibrarySongs([])
                        setSelectedSong(null)
                      }} />
                      <Button type="submit" variant="outlined" color="primary" startIcon={searchingLibrary ? <CircularProgress size={16} /> : <SearchIcon />}
                        disabled={searchingLibrary}>
                        Search library
                      </Button>
                    </form>

                    {librarySongs.length > 0 && (
                      <List disablePadding>
                        {librarySongs.map((song) => (
                          <ListItem key={song.id} button selected={selectedSong?.id === song.id} className={classes.song}
                            onClick={() => setSelectedSong(song)}>
                            <ListItemText
                              primary={song.title || 'Untitled'}
                              secondary={`${song.artist || 'Unknown artist'}${song.album ? ` · ${song.album}` : ''} · ${durationLabel(song.duration)}`}
                            />
                            {selectedSong?.id === song.id && <Chip size="small" color="primary" label="Destination" />}
                          </ListItem>
                        ))}
                      </List>
                    )}
                    {librarySearched && librarySongs.length === 0 && !searchingLibrary && (
                      <Typography variant="body2" color="textSecondary">No songs found in your private library.</Typography>
                    )}

                    <div className={classes.actions}>
                      <Button variant="contained" color="primary" disabled={saving || !selectedSong || !preview}
                        startIcon={saving ? <CircularProgress size={16} /> : <SaveIcon />} onClick={applyLyrics}>
                        {saving ? 'Saving…' : 'Save lyrics beside selected song'}
                      </Button>
                      {selectedSong && <Typography variant="caption" color="textSecondary">Destination: {selectedSong.title} - {selectedSong.artist}</Typography>}
                    </div>
                  </div>
                )}
              </Paper>
            )
          })
        )}

      </Paper>
    </div>
  )
}
