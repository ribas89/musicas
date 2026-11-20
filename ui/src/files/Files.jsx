import React, { useCallback, useEffect, useRef, useState } from 'react'
import { SimpleForm } from 'react-admin'
import {
  Box,
  Button,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  LinearProgress,
  List,
  ListItem,
  ListItemIcon,
  ListItemText,
  Paper,
  Typography,
  makeStyles,
} from '@material-ui/core'
import FolderIcon from '@material-ui/icons/Folder'
import InsertDriveFileIcon from '@material-ui/icons/InsertDriveFile'
import CreateNewFolderIcon from '@material-ui/icons/CreateNewFolder'
import CloudUploadIcon from '@material-ui/icons/CloudUpload'
import DeleteIcon from '@material-ui/icons/Delete'
import RefreshIcon from '@material-ui/icons/Refresh'
import ArrowBackIcon from '@material-ui/icons/ArrowBack'
import GetAppIcon from '@material-ui/icons/GetApp'
import { baseUrl } from '../utils'
import { useTranscodingOptions } from '../dialogs/useTranscodingOptions'

const MAX_FILES = 500
const SUPPORTED_EXTENSIONS = new Set(['mp3', 'flac', 'm4a', 'mp4', 'aac', 'ogg', 'opus', 'wav', 'aiff', 'aif', 'alac', 'wma', 'ape', 'wv', 'dsf', 'dff', 'mka', 'jpg', 'jpeg', 'png', 'webp', 'zip'])
const supportedFile = (name) => SUPPORTED_EXTENSIONS.has(name.split('.').pop().toLowerCase())
const useStyles = makeStyles((theme) => ({
  page: { padding: theme.spacing(3), maxWidth: 1100, margin: 'auto' },
  toolbar: { display: 'flex', gap: theme.spacing(1), flexWrap: 'wrap', margin: theme.spacing(2, 0) },
  dropzone: {
    border: '2px dashed', borderColor: theme.palette.divider, borderRadius: 8,
    textAlign: 'center', padding: theme.spacing(4), cursor: 'pointer',
    '&:hover': { borderColor: theme.palette.primary.main },
  },
  activeDrop: { borderColor: theme.palette.primary.main, backgroundColor: theme.palette.action.hover },
  file: { minWidth: 0 },
  actions: { marginLeft: 'auto', display: 'flex', flexWrap: 'wrap', gap: theme.spacing(1) },
  breadcrumbs: { wordBreak: 'break-word', marginTop: theme.spacing(1) },
  message: { margin: theme.spacing(1, 0), whiteSpace: 'pre-wrap' },
}))

const apiURL = (suffix = '') => baseUrl(`api/files/${suffix}`)
const parseResponse = async (response) => {
  const body = await response.json().catch(() => ({}))
  if (!response.ok) throw new Error(body.error || `HTTP ${response.status}`)
  return body
}
const makeHeaders = (json) => {
  const h = { 'X-ND-Authorization': `Bearer ${localStorage.getItem('token') || ''}` }
  if (json) h['Content-Type'] = 'application/json'
  return h
}
const request = async (suffix, options = {}) => parseResponse(await fetch(apiURL(suffix), {
  credentials: 'same-origin', headers: makeHeaders(Boolean(options.body)), ...options,
}))

// A browser FileList has no directory entries. Keep the browser-supplied
// relative paths separate from filenames so we can safely reconstruct folders
// on the server without trusting multipart filenames containing slashes.
const fromFileList = (files) => ({
  files: Array.from(files).filter((file) => supportedFile(file.name)).map((file) => ({ file, relativePath: file.webkitRelativePath || file.name })),
  directories: [],
})

const getFile = (entry) => new Promise((resolve, reject) => entry.file(resolve, reject))
const readEntries = (reader) => new Promise((resolve, reject) => reader.readEntries(resolve, reject))
const walkEntry = async (entry, parent, result) => {
  const relativePath = parent ? `${parent}/${entry.name}` : entry.name
  if (entry.isFile) {
    if (!supportedFile(entry.name)) return
    const file = await getFile(entry)
    result.files.push({ file, relativePath })
    if (result.files.length > MAX_FILES) throw new Error(`A maximum of ${MAX_FILES} files may be uploaded at once.`)
  } else if (entry.isDirectory) {
    result.directories.push(relativePath)
    if (result.directories.length > 2000) throw new Error('Too many nested directories')
    const reader = entry.createReader()
    // Chromium returns directory entries in batches (often 100 entries).
    // Keep reading until empty; a single readEntries loses large folders.
    while (true) {
      const batch = await readEntries(reader)
      if (!batch.length) break
      for (const child of batch) await walkEntry(child, relativePath, result)
    }
  }
}

const fromDrop = async (dataTransfer) => {
  const items = Array.from(dataTransfer.items || [])
  if (items.some((item) => item.kind === 'file' && typeof item.webkitGetAsEntry === 'function')) {
    const result = { files: [], directories: [] }
    // Capture entries synchronously during the drop event. The browser can
    // invalidate DataTransferItem access after the first asynchronous read.
    const entries = items.filter((item) => item.kind === 'file')
      .map((item) => item.webkitGetAsEntry()).filter(Boolean)
    for (const entry of entries) await walkEntry(entry, '', result)
    return entries.length ? result : fromFileList(dataTransfer.files)
  }
  // Browser does not expose FileSystemEntry: individual files are still
  // supported, but directory uploads require the Upload Folder picker.
  return fromFileList(dataTransfer.files)
}

const uploadWithProgress = (folderPath, selection, onProgress) => new Promise((resolve, reject) => {
  const form = new FormData()
  form.append('path', folderPath === '.' ? '' : folderPath)
  selection.directories.forEach((dir) => form.append('directories', dir))
  selection.files.forEach(({ file, relativePath }) => {
    form.append('files', file, file.name)
    form.append('relativePaths', relativePath)
  })
  const xhr = new XMLHttpRequest()
  xhr.open('POST', apiURL('upload'))
  xhr.setRequestHeader('X-ND-Authorization', `Bearer ${localStorage.getItem('token') || ''}`)
  xhr.upload.onprogress = (event) => {
    if (event.lengthComputable) onProgress(Math.round(event.loaded * 100 / event.total))
  }
  xhr.onerror = () => reject(new Error('Upload failed due to a network error'))
  xhr.onload = () => {
    let result = {}
    try { result = JSON.parse(xhr.responseText) } catch (_) { /* empty response */ }
    if (xhr.status >= 200 && xhr.status < 300) resolve(result)
    else reject(new Error(result.error || `HTTP ${xhr.status}`))
  }
  xhr.send(form)
})

const Files = () => {
  const classes = useStyles()
  const [folder, setFolder] = useState('.')
  const [entries, setEntries] = useState([])
  const [busy, setBusy] = useState(false)
  const [uploading, setUploading] = useState(false)
  const [progress, setProgress] = useState(0)
  const [scanning, setScanning] = useState(false)
  const [dragging, setDragging] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [downloadFolder, setDownloadFolder] = useState(null)
  const [downloading, setDownloading] = useState(false)
  const fileInput = useRef(null)
  const folderInput = useRef(null)

  const { TranscodingOptionsInput, originalFormat, format, maxBitRate } = useTranscodingOptions()

  const refresh = useCallback(async () => {
    setBusy(true)
    setError('')
    try {
      const result = await request(`?path=${encodeURIComponent(folder === '.' ? '' : folder)}`)
      setEntries(result.entries || [])
    } catch (err) { setError(err.message) } finally { setBusy(false) }
  }, [folder])
  useEffect(() => { refresh() }, [refresh])
  useEffect(() => {
    let active = true
    const check = async () => {
      try {
        const result = await request('scan')
        if (active) {
          setScanning(Boolean(result.scanning))
          if (result.error) setError(result.error)
        }
      } catch (_) { /* scan status is optional */ }
    }
    check()
    // Poll only during a scan, not forever when Files is idle.
    const timer = scanning ? setInterval(check, 2500) : null
    return () => { active = false; if (timer) clearInterval(timer) }
  }, [scanning])

  const openFolder = (name) => setFolder(folder === '.' ? name : `${folder}/${name}`)
  const parentFolder = () => setFolder(folder.includes('/') ? folder.split('/').slice(0, -1).join('/') : '.')

  const createFolder = async () => {
    const name = window.prompt('New folder name (top-level folders become playlists after a scan):')
    if (!name) return
    try {
      await request('folder', { method: 'POST', body: JSON.stringify({ path: folder === '.' ? '' : folder, name }) })
      setNotice(`Created ${name}`)
      refresh()
    } catch (err) { setError(err.message) }
  }

  const remove = async (item) => {
    if (!window.confirm(`Permanently delete ${item.name}${item.dir ? ' and everything inside it' : ''}?`)) return
    const target = folder === '.' ? item.name : `${folder}/${item.name}`
    try {
      await request(`?path=${encodeURIComponent(target)}`, { method: 'DELETE' })
      setNotice(`Deleted ${item.name}. Run Scan to update Navidrome.`)
      refresh()
    } catch (err) { setError(err.message) }
  }

  const upload = async (selection) => {
    if (uploading || (!selection.files.length && !selection.directories.length)) return
    if (selection.files.length > MAX_FILES) { setError(`Maximum ${MAX_FILES} files per upload`); return }
    setError('')
    setNotice('')
    setUploading(true)
    setProgress(0)
    try {
      const result = await uploadWithProgress(folder, selection, setProgress)
      setNotice(`Upload completed: ${result.uploaded?.length || 0} file(s), ${result.folders || 0} folder(s) created. Press Scan to update your library.`)
      await refresh()
    } catch (err) { setError(err.message) } finally { setUploading(false) }
  }

  const dropFiles = async (event) => {
    event.preventDefault()
    event.stopPropagation()
    setDragging(false)
    if (uploading) return
    try { await upload(await fromDrop(event.dataTransfer)) } catch (err) { setError(err.message) }
  }

  const scan = async () => {
    setError('')
    try {
      await request('scan', { method: 'POST' })
      setScanning(true)
      setNotice('Scanning only your private music library...')
    } catch (err) { setError(err.message) }
  }

  const download = async () => {
    if (!downloadFolder || downloading) return
    setDownloading(true)
    setError('')
    try {
      // The authenticated session endpoint provides a short-lived HttpOnly
      // download cookie. The browser can stream the response straight to disk.
      await request('download-session', { method: 'POST' })
      const params = new URLSearchParams({ path: downloadFolder, format: originalFormat ? 'raw' : format })
      if (!originalFormat) params.set('bitrate', maxBitRate?.toString() || '128')
      window.location.href = apiURL(`download?${params.toString()}`)
      setDownloadFolder(null)
    } catch (err) { setError(err.message) } finally { setDownloading(false) }
  }

  const sortedEntries = [...entries].sort((a, b) => Number(b.dir) - Number(a.dir) || a.name.localeCompare(b.name))
  return (
    <div className={classes.page}>
      <Typography variant="h5">Files</Typography>
      <Typography color="textSecondary">Manage your private library. Top-level folders become playlists after scanning.</Typography>
      <Typography className={classes.breadcrumbs} variant="body2" color="textSecondary">
        My music / {folder === '.' ? '' : folder.split('/').join(' / ')}
      </Typography>
      <div className={classes.toolbar}>
        {folder !== '.' && <Button variant="outlined" startIcon={<ArrowBackIcon />} onClick={parentFolder}>Back</Button>}
        <Button variant="outlined" startIcon={<CreateNewFolderIcon />} onClick={createFolder}>New folder</Button>
        <Button variant="outlined" startIcon={<CloudUploadIcon />} onClick={() => fileInput.current.click()} disabled={uploading}>Upload files / ZIP</Button>
        <Button variant="outlined" startIcon={<FolderIcon />} onClick={() => folderInput.current.click()} disabled={uploading}>Upload folder</Button>
        {folder !== '.' && <Button variant="outlined" startIcon={<GetAppIcon />} onClick={() => setDownloadFolder(folder)}>Download folder</Button>}
        <Button variant="outlined" startIcon={<RefreshIcon />} onClick={refresh}>Refresh</Button>
        <Button variant="contained" color="primary" startIcon={scanning ? <CircularProgress size={18} /> : <RefreshIcon />} onClick={scan} disabled={scanning || uploading}>
          {scanning ? 'Scanning...' : 'Scan my library'}
        </Button>
        <input type="file" ref={fileInput} multiple style={{ display: 'none' }} accept="audio/*,.mp3,.flac,.m4a,.opus,.ogg,.wav,.zip,.jpg,.jpeg,.png,.webp" onChange={(event) => { upload(fromFileList(event.target.files)); event.target.value = '' }} />
        <input type="file" ref={folderInput} multiple webkitdirectory="" directory="" style={{ display: 'none' }} onChange={(event) => { upload(fromFileList(event.target.files)); event.target.value = '' }} />
      </div>
      {error && <Typography color="error" className={classes.message}>{error}</Typography>}
      {notice && <Typography color="textSecondary" className={classes.message}>{notice}</Typography>}
      {uploading && <Box mb={2}><Typography variant="body2">Uploading: {progress}%</Typography><LinearProgress variant="determinate" value={progress} /></Box>}
      <Paper
        variant="outlined"
        className={`${classes.dropzone} ${dragging ? classes.activeDrop : ''}`}
        onDragOver={(e) => { e.preventDefault(); e.stopPropagation(); setDragging(true) }}
        onDragLeave={(e) => { e.preventDefault(); e.stopPropagation(); setDragging(false) }}
        onDrop={dropFiles}
        onClick={() => fileInput.current.click()}
      >
        <CloudUploadIcon fontSize="large" color="primary" />
        <Typography>Drag and drop music files, whole folders, or ZIP archives</Typography>
        <Typography variant="caption" color="textSecondary">Folders keep their hierarchy. ZIPs extract to a new folder. No existing music files are overwritten.</Typography>
      </Paper>
      <Box mt={2}>
        {busy ? <CircularProgress size={24} /> : (
          <List component="nav">
            {sortedEntries.length === 0 && <Typography color="textSecondary">This folder is empty.</Typography>}
            {sortedEntries.map((item) => (
              <ListItem key={item.name} divider button={item.dir} onClick={() => item.dir && openFolder(item.name)}>
                <ListItemIcon>{item.dir ? <FolderIcon /> : <InsertDriveFileIcon />}</ListItemIcon>
                <ListItemText className={classes.file} primary={item.name} secondary={item.dir ? 'Folder' : `${(item.size / 1024 / 1024).toFixed(2)} MB`} />
                <div className={classes.actions}>
                  {item.dir && <Button size="small" startIcon={<GetAppIcon />} onClick={(e) => { e.stopPropagation(); setDownloadFolder(folder === '.' ? item.name : `${folder}/${item.name}`) }}>Download</Button>}
                  <Button size="small" color="secondary" startIcon={<DeleteIcon />} onClick={(e) => { e.stopPropagation(); remove(item) }}>Delete</Button>
                </div>
              </ListItem>
            ))}
          </List>
        )}
      </Box>
      <Typography color="textSecondary" variant="caption">Folder downloads include only songs already indexed by Navidrome. After uploading, run Scan first.</Typography>
      <Dialog open={Boolean(downloadFolder)} onClose={() => setDownloadFolder(null)} fullWidth maxWidth="sm">
        <DialogTitle>Download folder: {downloadFolder?.split('/').pop()}</DialogTitle>
        <DialogContent>
          <SimpleForm toolbar={null} variant="outlined">
            <TranscodingOptionsInput fullWidth label="Download in original format" />
          </SimpleForm>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setDownloadFolder(null)} disabled={downloading}>Cancel</Button>
          <Button color="primary" onClick={download} disabled={downloading || (!originalFormat && !format)} startIcon={downloading && <CircularProgress size={16} />}>Download ZIP</Button>
        </DialogActions>
      </Dialog>
    </div>
  )
}

export default Files
