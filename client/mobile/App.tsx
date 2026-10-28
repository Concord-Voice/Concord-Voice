/**
 * Concord Voice mobile client (#206). UI comes later from the internal design
 * prototype; this shell only proves the app boots.
 */
import {
  StatusBar,
  StyleSheet,
  Text,
  useColorScheme,
  View,
} from 'react-native';

export default function App() {
  const dark = useColorScheme() === 'dark';
  return (
    <View style={[styles.root, dark ? styles.dark : styles.light]}>
      <StatusBar barStyle={dark ? 'light-content' : 'dark-content'} />
      <Text style={dark ? styles.darkText : styles.lightText}>
        Concord Voice
      </Text>
    </View>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1, alignItems: 'center', justifyContent: 'center' },
  dark: { backgroundColor: '#000' },
  light: { backgroundColor: '#fff' },
  darkText: { color: '#fff' },
  lightText: { color: '#000' },
});
