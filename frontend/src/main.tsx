import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom'
import './index.css'
import Setup from './pages/Setup'
import Cards from './pages/Cards'
import Board from './pages/Board'
import PRDashboard from './pages/PRDashboard'
import Brief from './pages/Brief'
import Settings from './pages/Settings'
import Prompts from './pages/Prompts'
import Repos from './pages/Repos'
import Shell from './Shell'
import AuthGate from './AuthGate'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <BrowserRouter>
      <Routes>
        <Route path="/setup" element={<Setup />} />
        <Route
          element={
            <AuthGate>
              <Shell />
            </AuthGate>
          }
        >
          <Route path="/" element={<Cards />} />
          <Route path="/board" element={<Board />} />
          <Route path="/prs" element={<PRDashboard />} />
          <Route path="/prompts" element={<Prompts />} />
          <Route path="/repos" element={<Repos />} />
          <Route path="/brief" element={<Brief />} />
          <Route path="/settings" element={<Settings />} />
        </Route>
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </BrowserRouter>
  </StrictMode>,
)
