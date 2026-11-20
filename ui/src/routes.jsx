import React from 'react'
import { Route } from 'react-router-dom'
import Personal from './personal/Personal'
import Files from './files/Files'
import YouTube from './youtube/YouTube'
import Lyrics from './lyrics/Lyrics'

const routes = [
  <Route exact path="/files" render={() => <Files />} key={'files'} />,
  <Route exact path="/youtube" render={() => <YouTube />} key={'youtube'} />,
  <Route exact path="/lyrics" render={() => <Lyrics />} key={'lyrics'} />,
  <Route exact path="/personal" render={() => <Personal />} key={'personal'} />,
]

export default routes
