import { createApp } from 'vue';
import App from './App.vue';
import { router } from './router';
import './styles/graph.css';
import './styles/style.css';
import './styles/llm.css';
import './styles/shell.css';
import './styles/blackboard.css';
import './styles/forms.css';
import './styles/inspector.css';

createApp(App).use(router).mount('#app');
